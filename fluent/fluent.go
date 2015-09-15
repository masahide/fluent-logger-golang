package fluent

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"reflect"
	"strconv"
	"time"

	"golang.org/x/net/context"
)

const (
	defaultHost                   = "127.0.0.1"
	defaultNetwork                = "tcp"
	defaultSocketPath             = ""
	defaultPort                   = 24224
	defaultTimeout                = 3 * time.Second
	defaultBufferLimit            = 8 * 1024 * 1024
	defaultRetryWait              = 500
	defaultMaxRetry               = 13
	defaultReconnectWaitIncreRate = 1.5
	defaultSyncPost               = false
)

type Config struct {
	FluentPort       int
	FluentHost       string
	FluentNetwork    string
	FluentSocketPath string
	Timeout          time.Duration
	BufferLimit      int
	RetryWait        int
	MaxRetry         int
	TagPrefix        string
	SyncPost         bool
}

type Fluent struct {
	Config
	conn   io.WriteCloser
	buf    []byte
	postCh chan []byte
	ctx    context.Context
	cancel context.CancelFunc
}

// New creates a new Logger.
func New(config Config) (f *Fluent, err error) {
	if config.FluentNetwork == "" {
		config.FluentNetwork = defaultNetwork
	}
	if config.FluentHost == "" {
		config.FluentHost = defaultHost
	}
	if config.FluentPort == 0 {
		config.FluentPort = defaultPort
	}
	if config.FluentSocketPath == "" {
		config.FluentSocketPath = defaultSocketPath
	}
	if config.Timeout == 0 {
		config.Timeout = defaultTimeout
	}
	if config.BufferLimit == 0 {
		config.BufferLimit = defaultBufferLimit
	}
	if config.RetryWait == 0 {
		config.RetryWait = defaultRetryWait
	}
	if config.MaxRetry == 0 {
		config.MaxRetry = defaultMaxRetry
	}
	if config.SyncPost == false {
		config.SyncPost = defaultSyncPost
	}
	f = &Fluent{
		Config: config,
		postCh: make(chan []byte),
	}

	f.ctx, f.cancel = context.WithCancel(context.Background())

	if err = f.connect(); err != nil {
		return
	}
	go f.spooler(f.ctx)

	return
}

// Post writes the output for a logging event.
//
// Examples:
//
//  // send string
//  f.Post("tag_name", "data")
//
//  // send map[string]
//  mapStringData := map[string]string{
//  	"foo":  "bar",
//  }
//  f.Post("tag_name", mapStringData)
//
//  // send message with specified time
//  mapStringData := map[string]string{
//  	"foo":  "bar",
//  }
//  tm := time.Now()
//  f.PostWithTime("tag_name", tm, mapStringData)
//
//  // send struct
//  structData := struct {
//  		Name string `msg:"name"`
//  } {
//  		"john smith",
//  }
//  f.Post("tag_name", structData)
//
func (f *Fluent) Post(tag string, message interface{}) error {
	timeNow := time.Now()
	return f.PostWithTime(tag, timeNow, message)
}

func (f *Fluent) PostWithTime(tag string, tm time.Time, message interface{}) error {
	if len(f.TagPrefix) > 0 {
		tag = f.TagPrefix + "." + tag
	}

	msg := reflect.ValueOf(message)
	msgtype := msg.Type()

	if msgtype.Kind() == reflect.Struct {
		// message should be tagged by "codec" or "msg"
		kv := make(map[string]interface{})
		fields := msgtype.NumField()
		for i := 0; i < fields; i++ {
			field := msgtype.Field(i)
			name := field.Name
			if n1 := field.Tag.Get("msg"); n1 != "" {
				name = n1
			} else if n2 := field.Tag.Get("codec"); n2 != "" {
				name = n2
			}
			kv[name] = msg.FieldByIndex(field.Index).Interface()
		}
		return f.EncodeAndPostData(tag, tm, kv)
	}

	if msgtype.Kind() != reflect.Map {
		return errors.New("messge must be a map")
	} else if msgtype.Key().Kind() != reflect.String {
		return errors.New("map keys must be strings")
	}

	kv := make(map[string]interface{})
	for _, k := range msg.MapKeys() {
		kv[k.String()] = msg.MapIndex(k).Interface()
	}

	return f.EncodeAndPostData(tag, tm, kv)
}

func (f *Fluent) EncodeAndPostData(tag string, tm time.Time, message interface{}) error {
	data, dumperr := f.EncodeData(tag, tm, message)
	if dumperr != nil {
		return fmt.Errorf("fluent#EncodeAndPostData: can't convert '%s' to msgpack:%s", message, dumperr)
		// fmt.Println("fluent#Post: can't convert to msgpack:", message, dumperr)
	}
	if f.SyncPost {
		return f.send(data)
	}
	f.PostRawData(data)
	return nil
}

func (f *Fluent) PostRawData(data []byte) {
	var buf []byte
	copy(buf, f.buf)
	f.postCh <- buf
}

func (f *Fluent) EncodeData(tag string, tm time.Time, message interface{}) (data []byte, err error) {
	timeUnix := tm.Unix()
	msg := &Message{Tag: tag, Time: timeUnix, Record: message}
	data, err = msg.MarshalMsg(nil)
	return
}

// Close closes the connection.
func (f *Fluent) Close() (err error) {
	f.cancel()
	return nil
}

// close closes the connection.
func (f *Fluent) close() (err error) {
	if f.conn == nil {
		return
	}
	f.conn.Close()
	f.conn = nil
	return
}

// connect establishes a new connection using the specified transport.
func (f *Fluent) connect() (err error) {
	switch f.Config.FluentNetwork {
	case "tcp":
		f.conn, err = net.DialTimeout(f.Config.FluentNetwork, f.Config.FluentHost+":"+strconv.Itoa(f.Config.FluentPort), f.Config.Timeout)
	case "unix":
		f.conn, err = net.DialTimeout(f.Config.FluentNetwork, f.Config.FluentSocketPath, f.Config.Timeout)
	default:
		err = net.UnknownNetworkError(f.Config.FluentNetwork)
	}
	return
}

func e(x, y float64) int {
	return int(math.Pow(x, y))
}

func (f *Fluent) reconnect() {
	go func() {
		for i := 0; ; i++ {
			err := f.connect()
			if err == nil {
				break
			}
			if i == f.Config.MaxRetry {
				panic("fluent#reconnect: failed to reconnect!")
			}
			waitTime := f.Config.RetryWait * e(defaultReconnectWaitIncreRate, float64(i-1))
			time.Sleep(time.Duration(waitTime) * time.Millisecond)
		}
	}()
}

func (f *Fluent) flushBuffer() {
	f.buf = f.buf[0:0]
}

func (f *Fluent) send(data []byte) (err error) {
	if f.conn == nil {
		f.reconnect()
	}
	_, err = f.conn.Write(f.buf)
	return
}

func (f *Fluent) spooler(ctx context.Context) {
	senderResult := make(chan error)
	sendChCh := f.sender(ctx, senderResult)
	for {
		select {
		case data := <-f.postCh:
			f.buf = append(f.buf, data...)
			if len(f.buf) > f.Config.BufferLimit {
				f.flushBuffer()
			}
		case sendCh := <-sendChCh:
			var buf []byte
			copy(buf, f.buf)
			f.flushBuffer()
			sendCh <- buf
		case <-ctx.Done():
			<-senderResult
			f.send(f.buf)
			f.flushBuffer()
			f.close()
			return
		}
	}
}
func (f *Fluent) sender(ctx context.Context, result chan error) chan chan []byte {
	sendCh := make(chan chan []byte)
	go func() {
		bufCh := make(chan []byte)
		for {
			sendCh <- bufCh
			select {
			case data := <-bufCh:
				f.send(data)
			case <-ctx.Done():
				result <- ctx.Err()
				return
			}
		}
	}()
	return sendCh
}
