package sampler

import (
	"fmt"
	"net"
	"strconv"

	"github.com/garyburd/redigo/redis"
)

// Options is a configuration struct that instructs the sampler pkg to sample
// the redis instance listening on a particular host/port with a specified
// number of random keys.
type Options struct {
	Host    string
	Port    int
	NumKeys int
}

// A ValueType represents the various data types that redis can store. The
// string representation of a ValueType matches what is returned from redis'
// `TYPE` command.
type ValueType string

var (
	// TypeString represents a redis string value
	TypeString ValueType = "string"
	// TypeSortedSet represents a redis sorted set value
	TypeSortedSet ValueType = "zset"
	// TypeSet represents a redis set value
	TypeSet ValueType = "set"
	// TypeHash represents a redis hash value
	TypeHash ValueType = "hash"
	// TypeList represents a redis list value
	TypeList ValueType = "list"
	// TypeUnknown means that the redis value type is undefined, and indicates an error
	TypeUnknown ValueType = "unknown"
)

// An Aggregator returns 0 or more arbitrary strings, to be used during a
// sampling operation as aggregation groups or "buckets". For example, an
// Aggregator that takes the first letter of the key would cause a Sampler to
// aggregate stats by each letter of the alphabet
type Aggregator interface {
	Groups(key string, valueType ValueType) []string
}

// The AggregatorFunc type is an adapter to allow the use of
// ordinary functions as Aggregators.  If f is a function
// with the appropriate signature, AggregatorFunc(f) is an
// Aggregator object that calls f.
type AggregatorFunc func(key string, valueType ValueType) []string

// Groups provides 0 or more groups to aggregate `key` to, when sampling redis keys.
func (f AggregatorFunc) Groups(key string, valueType ValueType) []string {
	return f(key, valueType)
}

// flush is a convenience func for flushing a redis pipeline, receiving the
// replies, and returning them, along with any error
func flush(conn redis.Conn) ([]interface{}, error) {
	return redis.Values(conn.Do(""))
}

// ensureEntry is a convenience func for obtaining the Stats instance for the
// specified `group`, creating a new one if no such entry already exists
func ensureEntry(m map[string]*Results, group string, init func() *Results) *Results {
	var stats *Results
	var ok bool
	if stats, ok = m[group]; !ok {
		stats = init()
		m[group] = stats
	}
	return stats
}

// randomKey obtains a random redis key and its ValueType from the supplied redis connection
func randomKey(conn redis.Conn) (key string, vt ValueType, err error) {
	key, err = redis.String(conn.Do("RANDOMKEY"))
	if err != nil {
		return key, TypeUnknown, err
	}

	typeStr, err := redis.String(conn.Do("TYPE", key))
	if err != nil {
		return key, TypeUnknown, err
	}

	return key, ValueType(typeStr), nil
}

func sampleString(key string, conn redis.Conn, aggregator Aggregator, stats map[string]*Results) error {
	val, err := redis.String(conn.Do("GET", key))
	if err != nil {
		return err
	}

	for _, agg := range aggregator.Groups(key, TypeString) {
		s := ensureEntry(stats, agg, NewResults)
		s.observeString(key, val)
	}
	return nil
}

func sampleList(key string, conn redis.Conn, aggregator Aggregator, stats map[string]*Results) error {
	// TODO: Let's not always get the first element, like the orig. sampler
	conn.Send("LLEN", key)
	conn.Send("LRANGE", key, 0, 0)
	replies, err := flush(conn)
	if err != nil {
		return err
	}

	if len(replies) >= 2 {
		l, err := redis.Int(replies[0], nil)
		ms, err := redis.Strings(replies[1], err)
		if err != nil {
			return err
		}

		for _, g := range aggregator.Groups(key, TypeList) {
			s := ensureEntry(stats, g, NewResults)
			s.observeList(key, l, ms[0])
		}
	}
	return nil
}

func sampleSet(key string, conn redis.Conn, aggregator Aggregator, stats map[string]*Results) error {
	conn.Send("SCARD", key)
	conn.Send("SRANDMEMBER", key)
	replies, err := flush(conn)
	if err != nil {
		return err
	}

	if len(replies) >= 2 {
		l, err := redis.Int(replies[0], nil)
		m, err := redis.String(replies[1], err)
		if err != nil {
			return err
		}

		for _, g := range aggregator.Groups(key, TypeSet) {
			s := ensureEntry(stats, g, NewResults)
			s.observeSet(key, l, m)
		}
	}
	return nil
}

func sampleSortedSet(key string, conn redis.Conn, aggregator Aggregator, stats map[string]*Results) error {
	conn.Send("ZCARD", key)
	// TODO: Let's not always get the first element, like the orig. sampler
	conn.Send("ZRANGE", key, 0, 0)
	replies, err := flush(conn)
	if err != nil {
		return err
	}

	if len(replies) >= 2 {
		l, err := redis.Int(replies[0], nil)
		ms, err := redis.Strings(replies[1], err)
		if err != nil {
			return err
		}

		for _, g := range aggregator.Groups(key, TypeSortedSet) {
			s := ensureEntry(stats, g, NewResults)
			s.observeSortedSet(key, l, ms[0])
		}
	}
	return nil
}

func sampleHash(key string, conn redis.Conn, aggregator Aggregator, stats map[string]*Results) error {
	conn.Send("HLEN", key)
	conn.Send("HKEYS", key)
	replies, err := flush(conn)
	if err != nil {
		return err
	}

	if len(replies) >= 2 {
		for _, g := range aggregator.Groups(key, TypeHash) {

			// TODO: Let's not always get the first hash field, like the orig. sampler
			l, err := redis.Int(replies[0], nil)
			fields, err := redis.Strings(replies[1], err)
			if err != nil {
				return err
			}
			val, err := redis.String(conn.Do("HGET", key, fields[0]))
			if err != nil {
				return err
			}
			s := ensureEntry(stats, g, NewResults)
			s.observeHash(key, l, fields[0], val)
		}
	}
	return nil
}

// Run performs the configured sampling operation against the redis instance,
// aggregating statistics using the provided Aggregator.  If any errors occurr,
// the sampling is short-circuited, and the error is returned.  In such a case,
// the results should be considered invalid.
func Run(opts Options, aggregator Aggregator) (map[string]*Results, error) {

	stats := make(map[string]*Results)
	var err error

	conn, err := redis.Dial("tcp", net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port)))
	if err != nil {
		return stats, err
	}

	interval := opts.NumKeys / 100
	if interval == 0 {
		interval = 100
	}
	lastInterval := 0

	for i := 0; i < opts.NumKeys; i++ {
		key, vt, err := randomKey(conn)
		if err != nil {
			return stats, err
		}

		if i/interval != lastInterval {
			fmt.Printf("sampled %d keys from redis at: %s:%d...\n", i, opts.Host, opts.Port)
			lastInterval = i / interval
		}

		switch ValueType(vt) {
		case TypeString:
			err = sampleString(key, conn, aggregator, stats)
			if err != nil {
				return stats, err
			}
		case TypeList:
			err = sampleList(key, conn, aggregator, stats)
			if err != nil {
				return stats, err
			}
		case TypeSet:
			err = sampleSet(key, conn, aggregator, stats)
			if err != nil {
				return stats, err
			}
		case TypeSortedSet:
			err = sampleSortedSet(key, conn, aggregator, stats)
			if err != nil {
				return stats, err
			}
		case TypeHash:
			err = sampleHash(key, conn, aggregator, stats)
			if err != nil {
				return stats, err
			}
		default:
			return stats, fmt.Errorf("unknown type for redis key: %s", key)
		}
	}
	return stats, nil
}