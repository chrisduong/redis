package redis

import (
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/internal"
	"github.com/go-redis/redis/internal/consistenthash"
	"github.com/go-redis/redis/internal/hashtag"
	"github.com/go-redis/redis/internal/pool"
)

var errRingShardsDown = errors.New("redis: all ring shards are down")

// RingOptions are used to configure a ring client and should be
// passed to NewRing.
type RingOptions struct {
	// Map of name => host:port addresses of ring shards.
	Addrs map[string]string

	// Frequency of PING commands sent to check shards availability.
	// Shard is considered down after 3 subsequent failed checks.
	HeartbeatFrequency time.Duration

	// Following options are copied from Options struct.

	OnConnect func(*Conn) error

	DB       int
	Password string

	MaxRetries      int
	MinRetryBackoff time.Duration
	MaxRetryBackoff time.Duration

	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	PoolSize           int
	PoolTimeout        time.Duration
	IdleTimeout        time.Duration
	IdleCheckFrequency time.Duration
}

func (opt *RingOptions) init() {
	if opt.HeartbeatFrequency == 0 {
		opt.HeartbeatFrequency = 500 * time.Millisecond
	}

	switch opt.MinRetryBackoff {
	case -1:
		opt.MinRetryBackoff = 0
	case 0:
		opt.MinRetryBackoff = 8 * time.Millisecond
	}
	switch opt.MaxRetryBackoff {
	case -1:
		opt.MaxRetryBackoff = 0
	case 0:
		opt.MaxRetryBackoff = 512 * time.Millisecond
	}
}

func (opt *RingOptions) clientOptions() *Options {
	return &Options{
		OnConnect: opt.OnConnect,

		DB:       opt.DB,
		Password: opt.Password,

		DialTimeout:  opt.DialTimeout,
		ReadTimeout:  opt.ReadTimeout,
		WriteTimeout: opt.WriteTimeout,

		PoolSize:           opt.PoolSize,
		PoolTimeout:        opt.PoolTimeout,
		IdleTimeout:        opt.IdleTimeout,
		IdleCheckFrequency: opt.IdleCheckFrequency,
	}
}

type ringShard struct {
	Client *Client
	down   int32
}

func (shard *ringShard) String() string {
	var state string
	if shard.IsUp() {
		state = "up"
	} else {
		state = "down"
	}
	return fmt.Sprintf("%s is %s", shard.Client, state)
}

func (shard *ringShard) IsDown() bool {
	const threshold = 3
	return atomic.LoadInt32(&shard.down) >= threshold
}

func (shard *ringShard) IsUp() bool {
	return !shard.IsDown()
}

// Vote votes to set shard state and returns true if state was changed.
func (shard *ringShard) Vote(up bool) bool {
	if up {
		changed := shard.IsDown()
		atomic.StoreInt32(&shard.down, 0)
		return changed
	}

	if shard.IsDown() {
		return false
	}

	atomic.AddInt32(&shard.down, 1)
	return shard.IsDown()
}

// Ring is a Redis client that uses constistent hashing to distribute
// keys across multiple Redis servers (shards). It's safe for
// concurrent use by multiple goroutines.
//
// Ring monitors the state of each shard and removes dead shards from
// the ring. When shard comes online it is added back to the ring. This
// gives you maximum availability and partition tolerance, but no
// consistency between different shards or even clients. Each client
// uses shards that are available to the client and does not do any
// coordination when shard state is changed.
//
// Ring should be used when you need multiple Redis servers for caching
// and can tolerate losing data when one of the servers dies.
// Otherwise you should use Redis Cluster.
type Ring struct {
	cmdable

	opt       *RingOptions
	nreplicas int

	mu         sync.RWMutex
	hash       *consistenthash.Map
	shards     map[string]*ringShard
	shardsList []*ringShard

	cmdsInfoOnce internal.Once
	cmdsInfo     map[string]*CommandInfo

	closed bool
}

func NewRing(opt *RingOptions) *Ring {
	const nreplicas = 100
	opt.init()
	ring := &Ring{
		opt:       opt,
		nreplicas: nreplicas,

		hash:   consistenthash.New(nreplicas, nil),
		shards: make(map[string]*ringShard),
	}
	ring.setProcessor(ring.Process)
	for name, addr := range opt.Addrs {
		clopt := opt.clientOptions()
		clopt.Addr = addr
		ring.addShard(name, NewClient(clopt))
	}
	go ring.heartbeat()
	return ring
}

func (c *Ring) addShard(name string, cl *Client) {
	shard := &ringShard{Client: cl}
	c.mu.Lock()
	c.hash.Add(name)
	c.shards[name] = shard
	c.shardsList = append(c.shardsList, shard)
	c.mu.Unlock()
}

// Options returns read-only Options that were used to create the client.
func (c *Ring) Options() *RingOptions {
	return c.opt
}

func (c *Ring) retryBackoff(attempt int) time.Duration {
	return internal.RetryBackoff(attempt, c.opt.MinRetryBackoff, c.opt.MaxRetryBackoff)
}

// PoolStats returns accumulated connection pool stats.
func (c *Ring) PoolStats() *PoolStats {
	c.mu.RLock()
	shards := c.shardsList
	c.mu.RUnlock()

	var acc PoolStats
	for _, shard := range shards {
		s := shard.Client.connPool.Stats()
		acc.Hits += s.Hits
		acc.Misses += s.Misses
		acc.Timeouts += s.Timeouts
		acc.TotalConns += s.TotalConns
		acc.FreeConns += s.FreeConns
	}
	return &acc
}

// Subscribe subscribes the client to the specified channels.
func (c *Ring) Subscribe(channels ...string) *PubSub {
	if len(channels) == 0 {
		panic("at least one channel is required")
	}

	shard, err := c.shardByKey(channels[0])
	if err != nil {
		// TODO: return PubSub with sticky error
		panic(err)
	}
	return shard.Client.Subscribe(channels...)
}

// PSubscribe subscribes the client to the given patterns.
func (c *Ring) PSubscribe(channels ...string) *PubSub {
	if len(channels) == 0 {
		panic("at least one channel is required")
	}

	shard, err := c.shardByKey(channels[0])
	if err != nil {
		// TODO: return PubSub with sticky error
		panic(err)
	}
	return shard.Client.PSubscribe(channels...)
}

// ForEachShard concurrently calls the fn on each live shard in the ring.
// It returns the first error if any.
func (c *Ring) ForEachShard(fn func(client *Client) error) error {
	c.mu.RLock()
	shards := c.shardsList
	c.mu.RUnlock()

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for _, shard := range shards {
		if shard.IsDown() {
			continue
		}

		wg.Add(1)
		go func(shard *ringShard) {
			defer wg.Done()
			err := fn(shard.Client)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}(shard)
	}
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func (c *Ring) cmdInfo(name string) *CommandInfo {
	err := c.cmdsInfoOnce.Do(func() error {
		c.mu.RLock()
		shards := c.shardsList
		c.mu.RUnlock()

		var firstErr error
		for _, shard := range shards {
			cmdsInfo, err := shard.Client.Command().Result()
			if err == nil {
				c.cmdsInfo = cmdsInfo
				return nil
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	})
	if err != nil {
		return nil
	}
	if c.cmdsInfo == nil {
		return nil
	}
	info := c.cmdsInfo[name]
	if info == nil {
		internal.Logf("info for cmd=%s not found", name)
	}
	return info
}

func (c *Ring) shardByKey(key string) (*ringShard, error) {
	key = hashtag.Key(key)

	c.mu.RLock()

	if c.closed {
		c.mu.RUnlock()
		return nil, pool.ErrClosed
	}

	name := c.hash.Get(key)
	if name == "" {
		c.mu.RUnlock()
		return nil, errRingShardsDown
	}

	shard := c.shards[name]
	c.mu.RUnlock()
	return shard, nil
}

func (c *Ring) randomShard() (*ringShard, error) {
	return c.shardByKey(strconv.Itoa(rand.Int()))
}

func (c *Ring) shardByName(name string) (*ringShard, error) {
	if name == "" {
		return c.randomShard()
	}

	c.mu.RLock()
	shard := c.shards[name]
	c.mu.RUnlock()
	return shard, nil
}

func (c *Ring) cmdShard(cmd Cmder) (*ringShard, error) {
	cmdInfo := c.cmdInfo(cmd.Name())
	pos := cmdFirstKeyPos(cmd, cmdInfo)
	if pos == 0 {
		return c.randomShard()
	}
	firstKey := cmd.stringArg(pos)
	return c.shardByKey(firstKey)
}

func (c *Ring) Process(cmd Cmder) error {
	shard, err := c.cmdShard(cmd)
	if err != nil {
		cmd.setErr(err)
		return err
	}
	return shard.Client.Process(cmd)
}

// rebalance removes dead shards from the Ring.
func (c *Ring) rebalance() {
	hash := consistenthash.New(c.nreplicas, nil)
	for name, shard := range c.shards {
		if shard.IsUp() {
			hash.Add(name)
		}
	}

	c.mu.Lock()
	c.hash = hash
	c.mu.Unlock()
}

// heartbeat monitors state of each shard in the ring.
func (c *Ring) heartbeat() {
	ticker := time.NewTicker(c.opt.HeartbeatFrequency)
	defer ticker.Stop()
	for range ticker.C {
		var rebalance bool

		c.mu.RLock()

		if c.closed {
			c.mu.RUnlock()
			break
		}

		shards := c.shardsList
		c.mu.RUnlock()

		for _, shard := range shards {
			err := shard.Client.Ping().Err()
			if shard.Vote(err == nil || err == pool.ErrPoolTimeout) {
				internal.Logf("ring shard state changed: %s", shard)
				rebalance = true
			}
		}

		if rebalance {
			c.rebalance()
		}
	}
}

// Close closes the ring client, releasing any open resources.
//
// It is rare to Close a Ring, as the Ring is meant to be long-lived
// and shared between many goroutines.
func (c *Ring) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true

	var firstErr error
	for _, shard := range c.shards {
		if err := shard.Client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	c.hash = nil
	c.shards = nil
	c.shardsList = nil

	return firstErr
}

func (c *Ring) Pipeline() Pipeliner {
	pipe := Pipeline{
		exec: c.pipelineExec,
	}
	pipe.setProcessor(pipe.Process)
	return &pipe
}

func (c *Ring) Pipelined(fn func(Pipeliner) error) ([]Cmder, error) {
	return c.Pipeline().Pipelined(fn)
}

func (c *Ring) pipelineExec(cmds []Cmder) error {
	cmdsMap := make(map[string][]Cmder)
	for _, cmd := range cmds {
		cmdInfo := c.cmdInfo(cmd.Name())
		name := cmd.stringArg(cmdFirstKeyPos(cmd, cmdInfo))
		if name != "" {
			name = c.hash.Get(hashtag.Key(name))
		}
		cmdsMap[name] = append(cmdsMap[name], cmd)
	}

	for attempt := 0; attempt <= c.opt.MaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(c.retryBackoff(attempt))
		}

		var failedCmdsMap map[string][]Cmder

		for name, cmds := range cmdsMap {
			shard, err := c.shardByName(name)
			if err != nil {
				setCmdsErr(cmds, err)
				continue
			}

			cn, _, err := shard.Client.getConn()
			if err != nil {
				setCmdsErr(cmds, err)
				continue
			}

			canRetry, err := shard.Client.pipelineProcessCmds(cn, cmds)
			if err == nil || internal.IsRedisError(err) {
				_ = shard.Client.connPool.Put(cn)
				continue
			}
			_ = shard.Client.connPool.Remove(cn)

			if canRetry && internal.IsRetryableError(err, true) {
				if failedCmdsMap == nil {
					failedCmdsMap = make(map[string][]Cmder)
				}
				failedCmdsMap[name] = cmds
			}
		}

		if len(failedCmdsMap) == 0 {
			break
		}
		cmdsMap = failedCmdsMap
	}

	return firstCmdsErr(cmds)
}

func (c *Ring) TxPipeline() Pipeliner {
	panic("not implemented")
}

func (c *Ring) TxPipelined(fn func(Pipeliner) error) ([]Cmder, error) {
	panic("not implemented")
}
