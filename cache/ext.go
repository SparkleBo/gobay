package cache

import (
	"encoding/json"
	"errors"
	"github.com/go-redis/redis"
	"github.com/shanbay/gobay"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// CacheExt 缓存扩展，提供了方便的缓存操作，可以选择backend
// 目前支持的backend有内存、redis。可以配置前缀，避免多个项目
// 共用一个redis实例时发生冲突。
type CacheExt struct {
	// gobay.Extension
	NS      string
	app     *gobay.Application
	backend CacheBackend
	prefix  string
}

// Init init a cache extension
func (c *CacheExt) Init(app *gobay.Application) error {
	c.app = app
	config := app.Config()
	if c.NS != "" {
		config = config.Sub(c.NS)
	}
	backend := config.GetString("cache_backend")
	if backend == "redis" {
		// redis backend
		c.prefix = config.GetString("cache_prefix")
		host := config.GetString("cache_host")
		password := config.GetString("cache_password")
		db_num := config.GetInt("cache_db")
		redisClient := redis.NewClient(&redis.Options{
			Addr:     host,
			Password: password,
			DB:       db_num,
		})
		_, err := redisClient.Ping().Result()
		if err != nil {
			return err
		}
		redisBack := new(redisBackend)
		redisBack.SetClient(redisClient)
		c.backend = redisBack

	} else {
		c.backend = new(memBackend)
		client := make(map[string]interface{})
		c.backend.SetClient(client)
	}
	return nil
}

// SetBackend 如果调用方想要自己定义backend，可以由这个方法设置进来
func (c *CacheExt) SetBackend(backend CacheBackend) {
	c.backend = backend
}

// MakeCacheKey 用于生成函数的缓存key，带版本控制。只允许数字、布尔、字符串这几种类似的参数。
// 使用#号拼接各个参数，尽量不要在字符串中出现#以避免碰撞
func (c *CacheExt) MakeCacheKey(f interface{}, version int, args ...interface{}) (string, error) {
	inputs := make([]string, len(args)+2)
	inputs[0] = runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
	inputs[1] = strconv.Itoa(version)
	for i, _ := range args {
		v := reflect.ValueOf(args[i])
		i += 2
		switch v.Kind() {
		case reflect.Invalid:
			return "", errors.New("invalid")
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			inputs[i] = strconv.FormatInt(v.Int(), 10)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			inputs[i] = strconv.FormatUint(v.Uint(), 10)
		case reflect.Bool:
			inputs[i] = strconv.FormatBool(v.Bool())
		case reflect.String:
			inputs[i] = v.String()
		default:
			return "", errors.New("Unsupported args type: " + v.Type().String())
		}
	}
	return strings.Join(inputs, "#"), nil
}

// CachedFunc 把一个正常函数变成一个缓存函数，类似python里的装饰器
// 缓存的key就是MakeCacheKey生成的key
// 对函数的要求：返回值有两个，第二个返回值是error 返回值不可以是nil
// 如果希望cache_none， 函数不返回nil即可
func (c *CacheExt) CachedFunc(function interface{}, ttl int64, version int) (func(args ...interface{}) (interface{}, error), error) {
	f_value := reflect.ValueOf(function)
	if f_value.Kind() != reflect.Func {
		return func(args ...interface{}) (interface{}, error) {
			return nil, nil
		}, errors.New("Generate func failed, the first param must be a function!")
	}
	if f_value.Type().NumOut() != 2 || !f_value.Type().Out(1).Implements(reflect.TypeOf((*error)(nil)).Elem()) {
		return func(args ...interface{}) (interface{}, error) {
			return nil, nil
		}, errors.New("Generate func failed, the response of function is invalid, should return (SomeStruct, error)")
	}
	return func(args ...interface{}) (interface{}, error) {
		cache_key, err := c.MakeCacheKey(function, version, args...)
		if err != nil {
			return nil, err
		}
		var isStruct bool = (f_value.Type().Out(0).Kind() == reflect.Struct || f_value.Type().Out(0).Kind() == reflect.Ptr)
		var cache_res interface{}
		var reCall bool
		if isStruct {
			cache_res = reflect.New(f_value.Type().Out(0))
			var exist bool
			exist, err = c.GetStruct(cache_key, &cache_res)
			if err != nil {
				return nil, err
			}
			if exist == false {
				reCall = true
			}
		} else {
			cache_res, err = c.Get(cache_key)
			if err != nil {
				return nil, err
			}
			if cache_res == nil {
				reCall = true
			}
		}
		if reCall { // 重新构建缓存
			inputs := make([]reflect.Value, len(args))
			for i, _ := range args {
				inputs[i] = reflect.ValueOf(args[i])
			}
			var outputs interface{} = f_value.Call(inputs)
			var call_res interface{} = outputs.([]reflect.Value)[0].Interface()
			if call_err, is_error := outputs.([]reflect.Value)[1].Interface().(error); is_error {
				return call_res, call_err
			}
			if call_res != nil {
				// 重置缓存
				cache_res = call_res
				if isStruct {
					err = c.SetStruct(cache_key, call_res, ttl)
					if err != nil {
						return call_res, err
					}
				} else {
					err = c.Set(cache_key, call_res, ttl)
					if err != nil {
						return call_res, err
					}
				}
			}
		}
		return cache_res, nil
	}, nil
}

// Close
func (c *CacheExt) Close() error {
	return c.backend.Close()
}

// Object
func (d *CacheExt) Object() interface{} {
	return d
}

// Application
func (d *CacheExt) Application() *gobay.Application {
	return d.app
}

func (c *CacheExt) trans_key(key string) string {
	return c.prefix + key
}

// Get 获取某个缓存key是对应的值，cache只能处理基本类型，
// 对于结构体需要调用方自行序列化、反序列化
func (c *CacheExt) Get(key string) (interface{}, error) {
	transed_key := c.trans_key(key)
	return c.backend.Get(transed_key)
}

// GetStruct  GetStruct("hello", &someStruce)
func (c *CacheExt) GetStruct(key string, m interface{}) (bool, error) {
	if reflect.ValueOf(m).Kind() != reflect.Ptr {
		return false, errors.New("Invalid param: m, want a struct's ptr")
	}
	transed_key := c.trans_key(key)
	data, err := c.backend.Get(transed_key)
	if data == nil {
		return false, err
	}
	err = json.Unmarshal([]byte(data.(string)), m)
	if err != nil {
		return true, err
	}
	return true, nil
}

func (c *CacheExt) validValue(value interface{}, isStruct bool) error {
	valueKind := reflect.ValueOf(value).Kind()
	if isStruct {
		switch valueKind {
		case reflect.Struct, reflect.Ptr:
		default:
			return errors.New("Struct value not supported type: " + valueKind.String())
		}
	} else {
		switch valueKind {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		case reflect.Bool:
		case reflect.String:
		default:
			return errors.New("Basic value not supported type: " + valueKind.String())
		}
	}
	return nil
}

// Set 设置某个缓存值，设置时必须要填写一个ttl，如果想要使用nx=True这样
// 的参数，可以使用redis实例。
func (c *CacheExt) Set(key string, value interface{}, ttl int64) error {
	if err := c.validValue(value, false); err != nil {
		return err
	}
	transed_key := c.trans_key(key)
	return c.backend.Set(transed_key, value, time.Duration(ttl)*time.Second)
}

// SetStruct
func (c *CacheExt) SetStruct(key string, value interface{}, ttl int64) error {
	if err := c.validValue(value, true); err != nil {
		return err
	}
	transed_key := c.trans_key(key)
	json_bytes, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.backend.Set(transed_key, string(json_bytes), time.Duration(ttl)*time.Second)
}

// SetMany MSet命令，会重置所有key的过期时间.
func (d *CacheExt) SetMany(keys []string, values []interface{}, ttl int64) error {
	transed_keys := make([]string, len(keys))
	for i, key := range keys {
		if err := d.validValue(values[i], false); err != nil {
			return err
		}
		transed_keys[i] = d.trans_key(key)
	}
	return d.backend.SetMany(transed_keys, values, time.Duration(ttl)*time.Second)
}

// GetMany
func (d *CacheExt) GetMany(keys []string) []interface{} {
	transed_keys := make([]string, len(keys))
	for i, key := range keys {
		transed_keys[i] = d.trans_key(key)
	}
	return d.backend.GetMany(transed_keys)
}

// GetManyStruct
// SetManyStruct

// Delete
func (d *CacheExt) Delete(key string) int64 {
	return d.backend.Delete(d.trans_key(key))
}

func (d *CacheExt) DeleteMany(keys []string) int64 {
	transed_keys := make([]string, len(keys))
	for i, key := range keys {
		transed_keys[i] = d.trans_key(key)
	}
	return d.backend.DeleteMany(transed_keys)
}

// Expire
func (d *CacheExt) Expire(key string, ttl int) bool {
	return d.backend.Expire(d.trans_key(key), time.Duration(ttl)*time.Second)
}

// TTL
func (d *CacheExt) TTL(key string) int64 {
	return d.backend.TTL(d.trans_key(key))
}

// Exists
func (d *CacheExt) Exists(key string) bool {
	return d.backend.Exists(d.trans_key(key))
}

// Clear
func (d *CacheExt) Clear() string {
	return d.backend.Clear()
}
