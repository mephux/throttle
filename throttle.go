package throttle

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-martini/martini"
)

const (
	// Too Many Requests According to http://tools.ietf.org/html/rfc6585#page-3
	StatusTooManyRequests = 429

	// The default Status Code used
	defaultStatusCode = StatusTooManyRequests

	// The default Message to include, defaults to 429 status code title
	defaultMessage = "Too Many Requests"

	// The default key prefix for Key Value Storage
	defaultKeyPrefix = "throttle"
)

type Options struct {
	// The status code to be returned for throttled requests
	// Defaults to 429 Too Many Requests
	StatusCode int

	// The message to be returned as the body of throttled requests
	Message string

	// The function used to identify the requester
	// Defaults to IP identification
	IdentificationFunction func(*http.Request) string

	// The key prefix to use in any key value store
	// defaults to "throttle"
	KeyPrefix string

	// The store to use
	// defaults to a simple concurrent-safe map[string]string
	Store KeyValueStorer
}

// KeyValueStorer is the required interface for the Store Option
// This should allow for either drop-in replacement with compatible libraries,
// or easy write-up of adapters
type KeyValueStorer interface {
	// Simple Get Function
	Get(key string) ([]byte, error)
	// Simple Set Function
	Set(key string, value []byte) error
}

// The Quota is Request Rates per Time for a given policy
type Quota struct {
	// The Request Limit
	Limit uint64
	// The time window for the request Limit
	Within time.Duration
}

func (q *Quota) KeyId() string {
	return strconv.FormatInt(int64(q.Within)/int64(q.Limit), 10)
}

// An access message to return to the user
type accessMessage struct {
	// The given status Code
	StatusCode int
	// The given message
	Message string
}

// Return a new access message with the properties given
func newAccessMessage(statusCode int, message string) *accessMessage {
	return &accessMessage{
		statusCode,
		message,
	}
}

// An access count for a single identified user.
// Will be stored in the key value store, 1 per Policy and User
type accessCount struct {
	Count    uint64        `json:"count"`
	Start    time.Time     `json:"start"`
	Duration time.Duration `json:"duration"`
}

// Determine if the count is still fresh
func (r accessCount) IsFresh() bool {
	return time.Now().UTC().Sub(r.Start) < r.Duration
}

// Increment the count when fresh, or reset and then increment when stale
func (r *accessCount) Increment() {
	if r.IsFresh() {
		r.Count++
	} else {
		r.Count = 1
		r.Start = time.Now().UTC()
	}
}

// Get the count
func (r *accessCount) GetCount() uint64 {
	if r.IsFresh() {
		return r.Count
	} else {
		return 0
	}
}

// Return a new access count with the given duration
func newAccessCount(duration time.Duration) *accessCount {
	return &accessCount{
		0,
		time.Now().UTC(),
		duration,
	}
}

// Unmarshal a stringified JSON respresentation of an access count
func accessCountFromBytes(accessCountBytes []byte) *accessCount {
	byteBufferString := bytes.NewBuffer(accessCountBytes)
	a := &accessCount{}
	if err := json.NewDecoder(byteBufferString).Decode(a); err != nil {
		panic(err.Error())
	}
	return a
}

// The controller, stores the allowed quota and has access to the store
type controller struct {
	quota *Quota
	store KeyValueStorer
}

// Get an access count by id
func (c *controller) GetAccessCount(id string) (a *accessCount) {
	accessCountBytes, err := c.store.Get(id)

	if err == nil {
		a = accessCountFromBytes(accessCountBytes)
	} else {
		a = newAccessCount(c.quota.Within)
	}

	return a
}

// Set an access count by id, will write to the store
func (c *controller) SetAccessCount(id string, a *accessCount) {
	marshalled, err := json.Marshal(a)
	if err != nil {
		panic(err.Error())
	}

	err = c.store.Set(id, marshalled)
	if err != nil {
		panic(err.Error())
	}
}

// Gets the access count, increments it and writes it back to the store
func (c *controller) RegisterAccess(id string) {
	counter := c.GetAccessCount(id)
	counter.Increment()
	c.SetAccessCount(id, counter)
}

// Check if the controller denies access for the given id based on
// the quota and used access
func (c *controller) DeniesAccess(id string) bool {
	counter := c.GetAccessCount(id)
	return counter.GetCount() >= c.quota.Limit
}

// Get a time for the given id when the quota time window will be reset
func (c *controller) RetryAt(id string) time.Time {
	counter := c.GetAccessCount(id)

	return counter.Start.Add(c.quota.Within)
}

// Get the remaining limit for the given id
func (c *controller) RemainingLimit(id string) uint64 {
	counter := c.GetAccessCount(id)

	return c.quota.Limit - counter.GetCount()
}

// Return a new controller with the given quota and store
func newController(quota *Quota, store KeyValueStorer) *controller {
	return &controller{
		quota,
		store,
	}
}

// Identify via the given Identification Function
func (o *Options) Identify(req *http.Request) string {
	return o.IdentificationFunction(req)
}

// A throttling Policy
// Takes two arguments, one required:
// First is a Quota (A Limit with an associated time). When the given Limit
// of requests is reached by a user within the given time window, access to
// access to resources will be denied to this user
// Second is Options to use with this policy. For further information on options,
// see Options further above.
func Policy(quota *Quota, options ...*Options) martini.Handler {
	o := newOptions(options)
	controller := newController(quota, o.Store)

	return func(context martini.Context, resp http.ResponseWriter, req *http.Request) {
		id := makeKey(o.KeyPrefix, quota.KeyId(), o.Identify(req))

		if controller.DeniesAccess(id) {
			msg := newAccessMessage(o.StatusCode, o.Message)
			setRateLimitHeaders(resp, controller, id)
			resp.WriteHeader(msg.StatusCode)
			resp.Write([]byte(msg.Message))
			return
		} else {
			controller.RegisterAccess(id)
			setRateLimitHeaders(resp, controller, id)
		}

	}
}

// Set Rate Limit Headers helper function
func setRateLimitHeaders(resp http.ResponseWriter, controller *controller, id string) {
	headers := resp.Header()
	headers.Set("X-RateLimit-Limit", strconv.FormatUint(controller.quota.Limit, 10))
	headers.Set("X-RateLimit-Reset", strconv.FormatInt(controller.RetryAt(id).Unix(), 10))
	headers.Set("X-RateLimit-Remaining", strconv.FormatUint(controller.RemainingLimit(id), 10))
}

// The default identifier function. Identifies a client by IP
func defaultIdentify(req *http.Request) string {
	if forwardedFor := req.Header.Get("X-FORWARDED-FOR"); forwardedFor != "" {
		return forwardedFor
	}

	ip, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		panic(err.Error())
	}
	return ip
}

// Make a key from various parts for use in the key value store
func makeKey(parts ...string) string {
	return strings.Join(parts, "_")
}

// Creates new default options and assigns any given options
func newOptions(options []*Options) *Options {
	o := Options{
		defaultStatusCode,
		defaultMessage,
		defaultIdentify,
		defaultKeyPrefix,
		nil,
	}

	// when all defaults, return it
	if len(options) == 0 {
		o.Store = NewMapStore(accessCount{})
		return &o
	}

	// map the given values to the options
	optionsValue := reflect.ValueOf(options[0])
	oValue := reflect.ValueOf(&o)
	numFields := optionsValue.Elem().NumField()

	for i := 0; i < numFields; i++ {
		if value := optionsValue.Elem().Field(i); value.IsValid() && value.CanSet() && isNonEmptyOption(value) {
			oValue.Elem().Field(i).Set(value)
		}
	}

	if o.Store == nil {
		o.Store = NewMapStore(accessCount{})
	}

	return &o
}

// Check if an option is assigned
func isNonEmptyOption(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return v.Len() != 0
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() != 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() != 0
	case reflect.Float32, reflect.Float64:
		return v.Float() != 0
	case reflect.Interface, reflect.Ptr:
		return !v.IsNil()
	}
	return false
}
