// Package memory implements the storage interface for a Conf
// BitTorrent tracker keeping peer data in memory.
package memory

import (
	"encoding/binary"
	"fmt"
	"math"
	"net/netip"
	"reflect"
	"runtime"
	"sync"
	"time"

	"github.com/sot-tech/mochi/bittorrent"
	"github.com/sot-tech/mochi/pkg/conf"
	"github.com/sot-tech/mochi/pkg/log"
	"github.com/sot-tech/mochi/pkg/metrics"
	"github.com/sot-tech/mochi/pkg/stop"
	"github.com/sot-tech/mochi/pkg/timecache"
	"github.com/sot-tech/mochi/storage"
)

// Default config constants.
const (
	// Name is the name by which this peer store is registered with Conf.
	Name              = "memory"
	defaultShardCount = 1024
)

func init() {
	// Register the storage driver.
	storage.RegisterBuilder(Name, Builder)
}

func Builder(icfg conf.MapConfig) (storage.PeerStorage, error) {
	var cfg Config
	if err := icfg.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	return NewPeerStorage(cfg)
}

// Config holds the configuration of a memory PeerStorage.
type Config struct {
	ShardCount int `cfg:"shard_count"`
}

// LogFields renders the current config as a set of Logrus fields.
func (cfg Config) LogFields() log.Fields {
	return log.Fields{
		"Name":       Name,
		"ShardCount": cfg.ShardCount,
	}
}

// Validate sanity checks values set in a config and returns a new config with
// default values replacing anything that is invalid.
//
// This function warns to the logger when a value is changed.
func (cfg Config) Validate() Config {
	validcfg := cfg

	if cfg.ShardCount <= 0 || cfg.ShardCount > (math.MaxInt/2) {
		validcfg.ShardCount = defaultShardCount
		log.Warn("falling back to default configuration", log.Fields{
			"Name":     Name + ".ShardCount",
			"Provided": cfg.ShardCount,
			"Default":  validcfg.ShardCount,
		})
	}

	return validcfg
}

// NewPeerStorage creates a new PeerStorage backed by memory.
func NewPeerStorage(provided Config) (storage.PeerStorage, error) {
	cfg := provided.Validate()
	ps := &peerStore{
		cfg:         cfg,
		shards:      make([]*peerShard, cfg.ShardCount*2),
		DataStorage: NewDataStorage(),
		closed:      make(chan struct{}),
	}

	for i := 0; i < cfg.ShardCount*2; i++ {
		ps.shards[i] = &peerShard{swarms: make(map[bittorrent.InfoHash]swarm)}
	}

	return ps, nil
}

type peerShard struct {
	swarms      map[bittorrent.InfoHash]swarm
	numSeeders  uint64
	numLeechers uint64
	sync.RWMutex
}

type swarm struct {
	// map serialized peer to mtime
	seeders  map[string]int64
	leechers map[string]int64
}

type peerStore struct {
	storage.DataStorage
	cfg    Config
	shards []*peerShard

	closed chan struct{}
	wg     sync.WaitGroup
}

var _ storage.PeerStorage = &peerStore{}

func (ps *peerStore) ScheduleGC(gcInterval, peerLifeTime time.Duration) {
	ps.wg.Add(1)
	go func() {
		defer ps.wg.Done()
		t := time.NewTimer(gcInterval)
		defer t.Stop()
		for {
			select {
			case <-ps.closed:
				return
			case <-t.C:
				before := time.Now().Add(-peerLifeTime)
				log.Debug("storage: Memory purging peers with no announces since", log.Fields{"before": before})
				start := time.Now()
				ps.gc(before)
				recordGCDuration(time.Since(start))
			}
		}
	}()
}

func (ps *peerStore) ScheduleStatisticsCollection(reportInterval time.Duration) {
	ps.wg.Add(1)
	go func() {
		defer ps.wg.Done()
		t := time.NewTicker(reportInterval)
		for {
			select {
			case <-ps.closed:
				t.Stop()
				return
			case <-t.C:
				if metrics.Enabled() {
					before := time.Now()
					// aggregates metrics over all shards and then posts them to
					// prometheus.
					var numInfohashes, numSeeders, numLeechers uint64

					for _, s := range ps.shards {
						s.RLock()
						numInfohashes += uint64(len(s.swarms))
						numSeeders += s.numSeeders
						numLeechers += s.numLeechers
						s.RUnlock()
					}

					storage.PromInfoHashesCount.Set(float64(numInfohashes))
					storage.PromSeedersCount.Set(float64(numSeeders))
					storage.PromLeechersCount.Set(float64(numLeechers))
					log.Debug("storage: Memory: populateProm() finished", log.Fields{"timeTaken": time.Since(before)})
				}
			}
		}
	}()
}

// recordGCDuration records the duration of a GC sweep.
func recordGCDuration(duration time.Duration) {
	storage.PromGCDurationMilliseconds.Observe(float64(duration.Nanoseconds()) / float64(time.Millisecond))
}

func (ps *peerStore) getClock() int64 {
	return timecache.NowUnixNano()
}

func (ps *peerStore) shardIndex(infoHash bittorrent.InfoHash, addr netip.Addr) uint32 {
	// There are twice the amount of shards specified by the user, the first
	// half is dedicated to IPv4 swarms and the second half is dedicated to
	// IPv6 swarms.
	idx := binary.BigEndian.Uint32([]byte(infoHash[:4])) % (uint32(len(ps.shards)) / 2)
	if addr.Is6() {
		idx += uint32(len(ps.shards) / 2)
	}
	return idx
}

func (ps *peerStore) PutSeeder(ih bittorrent.InfoHash, p bittorrent.Peer) error {
	select {
	case <-ps.closed:
		panic("attempted to interact with stopped memory store")
	default:
	}

	pk := p.RawString()

	shard := ps.shards[ps.shardIndex(ih, p.Addr())]
	shard.Lock()

	if _, ok := shard.swarms[ih]; !ok {
		shard.swarms[ih] = swarm{
			seeders:  make(map[string]int64),
			leechers: make(map[string]int64),
		}
	}

	// If this peer isn't already a seeder, update the stats for the swarm.
	if _, ok := shard.swarms[ih].seeders[pk]; !ok {
		shard.numSeeders++
	}

	// Update the peer in the swarm.
	shard.swarms[ih].seeders[pk] = ps.getClock()

	shard.Unlock()
	return nil
}

func (ps *peerStore) DeleteSeeder(ih bittorrent.InfoHash, p bittorrent.Peer) error {
	select {
	case <-ps.closed:
		panic("attempted to interact with stopped memory store")
	default:
	}

	pk := p.RawString()

	shard := ps.shards[ps.shardIndex(ih, p.Addr())]
	shard.Lock()

	if _, ok := shard.swarms[ih]; !ok {
		shard.Unlock()
		return storage.ErrResourceDoesNotExist
	}

	if _, ok := shard.swarms[ih].seeders[pk]; !ok {
		shard.Unlock()
		return storage.ErrResourceDoesNotExist
	}

	shard.numSeeders--
	delete(shard.swarms[ih].seeders, pk)

	if len(shard.swarms[ih].seeders)|len(shard.swarms[ih].leechers) == 0 {
		delete(shard.swarms, ih)
	}

	shard.Unlock()
	return nil
}

func (ps *peerStore) PutLeecher(ih bittorrent.InfoHash, p bittorrent.Peer) error {
	select {
	case <-ps.closed:
		panic("attempted to interact with stopped memory store")
	default:
	}

	pk := p.RawString()

	shard := ps.shards[ps.shardIndex(ih, p.Addr())]
	shard.Lock()

	if _, ok := shard.swarms[ih]; !ok {
		shard.swarms[ih] = swarm{
			seeders:  make(map[string]int64),
			leechers: make(map[string]int64),
		}
	}

	// If this peer isn't already a leecher, update the stats for the swarm.
	if _, ok := shard.swarms[ih].leechers[pk]; !ok {
		shard.numLeechers++
	}

	// Update the peer in the swarm.
	shard.swarms[ih].leechers[pk] = ps.getClock()

	shard.Unlock()
	return nil
}

func (ps *peerStore) DeleteLeecher(ih bittorrent.InfoHash, p bittorrent.Peer) error {
	select {
	case <-ps.closed:
		panic("attempted to interact with stopped memory store")
	default:
	}

	pk := p.RawString()

	shard := ps.shards[ps.shardIndex(ih, p.Addr())]
	shard.Lock()

	if _, ok := shard.swarms[ih]; !ok {
		shard.Unlock()
		return storage.ErrResourceDoesNotExist
	}

	if _, ok := shard.swarms[ih].leechers[pk]; !ok {
		shard.Unlock()
		return storage.ErrResourceDoesNotExist
	}

	shard.numLeechers--
	delete(shard.swarms[ih].leechers, pk)

	if len(shard.swarms[ih].seeders)|len(shard.swarms[ih].leechers) == 0 {
		delete(shard.swarms, ih)
	}

	shard.Unlock()
	return nil
}

func (ps *peerStore) GraduateLeecher(ih bittorrent.InfoHash, p bittorrent.Peer) error {
	select {
	case <-ps.closed:
		panic("attempted to interact with stopped memory store")
	default:
	}

	pk := p.RawString()

	shard := ps.shards[ps.shardIndex(ih, p.Addr())]
	shard.Lock()

	if _, ok := shard.swarms[ih]; !ok {
		shard.swarms[ih] = swarm{
			seeders:  make(map[string]int64),
			leechers: make(map[string]int64),
		}
	}

	// If this peer is a leecher, update the stats for the swarm and remove them.
	if _, ok := shard.swarms[ih].leechers[pk]; ok {
		shard.numLeechers--
		delete(shard.swarms[ih].leechers, pk)
	}

	// If this peer isn't already a seeder, update the stats for the swarm.
	if _, ok := shard.swarms[ih].seeders[pk]; !ok {
		shard.numSeeders++
	}

	// Update the peer in the swarm.
	shard.swarms[ih].seeders[pk] = ps.getClock()

	shard.Unlock()
	return nil
}

func (ps *peerStore) AnnouncePeers(ih bittorrent.InfoHash, seeder bool, numWant int, peer bittorrent.Peer) (peers []bittorrent.Peer, err error) {
	select {
	case <-ps.closed:
		panic("attempted to interact with stopped memory store")
	default:
	}

	shard := ps.shards[ps.shardIndex(ih, peer.Addr())]
	shard.RLock()

	if _, ok := shard.swarms[ih]; !ok {
		shard.RUnlock()
		return nil, storage.ErrResourceDoesNotExist
	}

	if seeder {
		// Append leechers as possible.
		leechers := shard.swarms[ih].leechers
		for pk := range leechers {
			if numWant == 0 {
				break
			}
			p, _ := bittorrent.NewPeer(pk)
			peers = append(peers, p)
			numWant--
		}
	} else {
		// Append as many seeders as possible.
		seeders := shard.swarms[ih].seeders
		for pk := range seeders {
			if numWant == 0 {
				break
			}
			p, _ := bittorrent.NewPeer(pk)
			peers = append(peers, p)
			numWant--
		}

		// Append leechers until we reach numWant.
		if numWant > 0 {
			leechers := shard.swarms[ih].leechers
			announcerPK := peer.RawString()
			for pk := range leechers {
				if pk == announcerPK {
					continue
				}

				if numWant == 0 {
					break
				}
				p, _ := bittorrent.NewPeer(pk)
				peers = append(peers, p)
				numWant--
			}
		}
	}

	shard.RUnlock()
	return
}

func (ps *peerStore) ScrapeSwarm(ih bittorrent.InfoHash, peer bittorrent.Peer) (resp bittorrent.Scrape) {
	select {
	case <-ps.closed:
		panic("attempted to interact with stopped memory store")
	default:
	}

	resp.InfoHash = ih
	shard := ps.shards[ps.shardIndex(ih, peer.Addr())]
	shard.RLock()

	swarm, ok := shard.swarms[ih]
	if !ok {
		shard.RUnlock()
		return
	}

	resp.Incomplete = uint32(len(swarm.leechers))
	resp.Complete = uint32(len(swarm.seeders))
	shard.RUnlock()

	return
}

// NewDataStorage creates new in-memory data store
func NewDataStorage() storage.DataStorage {
	return new(dataStore)
}

type dataStore struct {
	sync.Map
}

func asKey(in any) any {
	if in == nil {
		panic("unable to use nil map key")
	}
	if reflect.TypeOf(in).Comparable() {
		return in
	}
	// FIXME: dirty hack
	return fmt.Sprint(in)
}

func (ds *dataStore) Put(ctx string, values ...storage.Entry) error {
	if len(values) > 0 {
		c, _ := ds.LoadOrStore(ctx, new(sync.Map))
		m := c.(*sync.Map)
		for _, p := range values {
			m.Store(asKey(p.Key), p.Value)
		}
	}
	return nil
}

func (ds *dataStore) Contains(ctx string, key string) (bool, error) {
	var exist bool
	if m, found := ds.Map.Load(ctx); found {
		_, exist = m.(*sync.Map).Load(asKey(key))
	}
	return exist, nil
}

func (ds *dataStore) Load(ctx string, key string) (any, error) {
	var v any
	if m, found := ds.Map.Load(ctx); found {
		v, _ = m.(*sync.Map).Load(asKey(key))
	}
	return v, nil
}

func (ds *dataStore) Delete(ctx string, keys ...string) error {
	if len(keys) > 0 {
		if m, found := ds.Map.Load(ctx); found {
			m := m.(*sync.Map)
			for _, k := range keys {
				m.Delete(asKey(k))
			}
		}
	}
	return nil
}

func (*dataStore) Preservable() bool {
	return false
}

// GC deletes all Peers from the PeerStorage which are older than the
// cutoff time.
//
// This function must be able to execute while other methods on this interface
// are being executed in parallel.
func (ps *peerStore) gc(cutoff time.Time) {
	select {
	case <-ps.closed:
		return
	default:
	}

	cutoffUnix := cutoff.UnixNano()

	for _, shard := range ps.shards {
		shard.RLock()
		var infohashes []bittorrent.InfoHash
		for ih := range shard.swarms {
			infohashes = append(infohashes, ih)
		}
		shard.RUnlock()
		runtime.Gosched()

		for _, ih := range infohashes {
			shard.Lock()

			if _, stillExists := shard.swarms[ih]; !stillExists {
				shard.Unlock()
				runtime.Gosched()
				continue
			}

			for pk, mtime := range shard.swarms[ih].leechers {
				if mtime <= cutoffUnix {
					shard.numLeechers--
					delete(shard.swarms[ih].leechers, pk)
				}
			}

			for pk, mtime := range shard.swarms[ih].seeders {
				if mtime <= cutoffUnix {
					shard.numSeeders--
					delete(shard.swarms[ih].seeders, pk)
				}
			}

			if len(shard.swarms[ih].seeders)|len(shard.swarms[ih].leechers) == 0 {
				delete(shard.swarms, ih)
			}

			shard.Unlock()
			runtime.Gosched()
		}

		runtime.Gosched()
	}
}

func (ps *peerStore) Stop() stop.Result {
	c := make(stop.Channel)
	go func() {
		if ps.closed != nil {
			close(ps.closed)
		}
		ps.wg.Wait()

		// Explicitly deallocate our storage.
		shards := make([]*peerShard, len(ps.shards))
		for i := 0; i < len(ps.shards); i++ {
			shards[i] = &peerShard{swarms: make(map[bittorrent.InfoHash]swarm)}
		}
		ps.shards = shards

		c.Done()
	}()

	return c.Result()
}

func (ps *peerStore) LogFields() log.Fields {
	return ps.cfg.LogFields()
}
