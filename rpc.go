// SPDX-License-Identifier: MIT

package muxrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/karrick/bufpool"
	"github.com/pkg/errors"
	"go.cryptoscope.co/luigi"
	"go.cryptoscope.co/muxrpc/codec"
)

var (
	_ Endpoint = (*rpc)(nil)
	_ Server   = (*rpc)(nil)
)

// rpc implements an Endpoint, but also implements Server
type rpc struct {
	logger log.Logger

	remote net.Addr

	// pkr (un)marshales codec.Packets
	pkr *Packer

	bpool bufpool.FreeList

	// reqs is the map we keep, tracking all requests
	reqs  map[int32]*Request
	rLock sync.Mutex

	// highest is the highest request id we already allocated
	highest int32

	root Handler

	// terminated indicates that the rpc session is being terminated
	terminated bool
	tLock      sync.Mutex

	cancel context.CancelFunc
}

// this sets the buffer size of individual request streams
// if the other side of stream doesn't read it's messages
// the read loop stops draining and all other replies might deadlock
// BUG: too big settings can accumulate lot's of memory!
// think blobs.get, the whole blob might be held in memory before it's drained to the store
// (which might decide it's too big, at that point it was already received though....)
const bufSize = 150

// Handle handles the connection of the packer using the specified handler.
func Handle(pkr *Packer, handler Handler) Endpoint {
	return handle(pkr, handler, nil, nil)
}

// HandleWithRemote also sets the remote address the endpoint is connected to
// TODO: better passing through packer maybe?!
func HandleWithRemote(pkr *Packer, handler Handler, addr net.Addr) Endpoint {
	return handle(pkr, handler, addr, nil)
}

// HandleWithLogger same as Handle but let's you overwrite the stderr logger
func HandleWithLogger(pkr *Packer, handler Handler, logger log.Logger) Endpoint {
	return handle(pkr, handler, nil, logger)
}

func handle(pkr *Packer, handler Handler, remote net.Addr, logger log.Logger) Endpoint {
	if logger == nil {
		logger = log.NewLogfmtLogger(os.Stderr)
		logger = level.NewFilter(logger, level.AllowInfo()) // only log info and above
		logger = log.With(logger, "ts", log.DefaultTimestampUTC, "unit", "muxrpc")
	}
	if remote == nil {
		if ra, ok := pkr.c.(interface{ RemoteAddr() net.Addr }); ok {
			remote = ra.RemoteAddr()
		}
	}
	if remote != nil {
		logger = log.With(logger, "remote", remote.String())
	}

	// TODO: rpc root context!? serve context?!
	ctx := context.TODO()

	ctx, cancel := context.WithCancel(ctx)
	r := &rpc{
		logger: logger,
		remote: remote,
		pkr:    pkr,
		reqs:   make(map[int32]*Request),
		root:   handler,

		cancel: cancel,
	}

	bp, err := bufpool.NewChanPool()
	if err != nil {
		panic(err)
	}
	r.bpool = bp

	go handler.HandleConnect(ctx, r)

	return r
}

// no args should be handled as empty array not args: null
func marshalCallArgs(args []interface{}) ([]byte, error) {
	var argData []byte
	if len(args) == 0 {
		argData = []byte("[]")
	} else {
		var err error
		argData, err = json.Marshal(args)
		if err != nil {
			return nil, errors.Wrap(err, "error marshaling request arguments")
		}
	}
	return argData, nil
}

// Async does an aync call on the remote.
func (r *rpc) Async(ctx context.Context, tipe interface{}, method Method, args ...interface{}) (interface{}, error) {
	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	argData, err := marshalCallArgs(args)
	if err != nil {
		return nil, err
	}

	req := &Request{
		Type:   "async",
		Stream: newStream(inSrc, r.pkr, 0, streamCapOnce, streamCapNone),
		in:     inSink,

		Method:  method,
		RawArgs: argData,

		tipe: tipe,
	}

	if err := r.Do(ctx, req); err != nil {
		return nil, errors.Wrap(err, "error sending request")
	}
	v, err := req.Stream.Next(ctx)
	return v, errors.Wrap(err, "error reading response from request source")
}

// Source does a source call on the remote.
func (r *rpc) Source(ctx context.Context, tipe interface{}, method Method, args ...interface{}) (luigi.Source, error) {
	argData, err := marshalCallArgs(args)
	if err != nil {
		return nil, err
	}

	bs := NewByteSource(ctx)

	req := &Request{
		Type:    "source",
		Stream:  bs.AsStream(),
		consume: bs.consume,
		done:    bs.Cancel,

		//		Stream: newStream(inSrc, r.pkr, 0, streamCapMultiple, streamCapNone),
		//in:     inSink,

		Method:  method,
		RawArgs: argData,

		tipe: tipe,
	}

	if err := r.Do(ctx, req); err != nil {
		return nil, errors.Wrap(err, "error sending request")
	}

	return req.Stream, nil
}

func (r *rpc) ByteSource(ctx context.Context, method Method, args ...interface{}) (ByteSource, error) {

	argData, err := marshalCallArgs(args)
	if err != nil {
		return nil, err
	}

	//bs := NewByteSource(ctx)
	var bs = newByteSource()

	req := &Request{
		Type:   "source",
		Stream: bs.AsStream(), // stub for legacy interfaces

		consume: bs.consume,
		done:    bs.Cancel,

		Method:  method,
		RawArgs: argData,
	}

	if err := r.Do(ctx, req); err != nil {
		return nil, errors.Wrap(err, "error sending request")
	}

	return bs, nil
}

// Sink does a sink call on the remote.
func (r *rpc) Sink(ctx context.Context, method Method, args ...interface{}) (luigi.Sink, error) {
	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	argData, err := marshalCallArgs(args)
	if err != nil {
		return nil, err
	}

	req := &Request{
		Type:   "sink",
		Stream: newStream(inSrc, r.pkr, 0, streamCapNone, streamCapMultiple),
		in:     inSink,

		Method:  method,
		RawArgs: argData,
	}

	if err := r.Do(ctx, req); err != nil {
		return nil, errors.Wrap(err, "error sending request")
	}

	return req.Stream, nil
}

// Duplex does a duplex call on the remote.
func (r *rpc) Duplex(ctx context.Context, tipe interface{}, method Method, args ...interface{}) (luigi.Source, luigi.Sink, error) {
	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	argData, err := marshalCallArgs(args)
	if err != nil {
		return nil, nil, err
	}

	req := &Request{
		Type:   "duplex",
		Stream: newStream(inSrc, r.pkr, 0, streamCapMultiple, streamCapMultiple),
		in:     inSink,

		Method:  method,
		RawArgs: argData,

		tipe: tipe,
	}

	if err := r.Do(ctx, req); err != nil {
		return nil, nil, errors.Wrap(err, "error sending request")
	}

	return req.Stream, req.Stream, nil
}

var ErrSessionTerminated = errors.New("muxrpc: session terminated")

// Terminate ends the RPC session
func (r *rpc) Terminate() error {
	r.cancel()
	r.tLock.Lock()
	defer r.tLock.Unlock()
	r.terminated = true
	r.rLock.Lock()
	defer r.rLock.Unlock()
	if n := len(r.reqs); n > 0 { // close active requests
		for _, req := range r.reqs {
			req.CloseWithError(ErrSessionTerminated)
		}
	}
	return r.pkr.Close()
}

// Do executes a generic call
func (r *rpc) Do(ctx context.Context, req *Request) error {
	dbg := level.Debug(r.logger)
	dbg = log.With(dbg, "call", req.Type, "method", req.Method.String())
	if req.abort == nil {
		req.abort = func() {} // noop
	}

	var (
		pkt codec.Packet
		err error
	)

	if req.RawArgs == nil {
		req.RawArgs = []byte("[]")
	}

	func() {
		r.rLock.Lock()
		defer r.rLock.Unlock()

		pkt.Flag = pkt.Flag.Set(codec.FlagJSON)
		pkt.Flag = pkt.Flag.Set(req.Type.Flags())

		pkt.Body, err = json.Marshal(req)

		r.highest++
		pkt.Req = r.highest
		r.reqs[pkt.Req] = req
		req.Stream.WithReq(pkt.Req)
		req.Stream.WithType(req.tipe)

		req.id = pkt.Req
	}()
	if err != nil {
		dbg.Log("event", "request create failed", "reqID", req.id, "err", err)
		return err
	}

	err = r.pkr.Pour(ctx, &pkt)
	dbg.Log("event", "request sent", "reqID", req.id, "err", err)
	return err
}

// ParseRequest parses the first packet of a stream and parses the contained request
func (r *rpc) ParseRequest(pkt *codec.Header) (*Request, error) {
	var req Request

	if !pkt.Flag.Get(codec.FlagJSON) {
		return nil, errors.New("expected JSON flag")
	}

	if pkt.Req >= 0 {
		// request numbers should have been inverted by now
		return nil, errors.New("expected negative request id")
	}

	rd := r.pkr.r.NextBodyReader(pkt.Len)

	err := json.NewDecoder(rd).Decode(&req)
	if err != nil {
		return nil, errors.Wrap(err, "error decoding packet")
	}
	req.id = pkt.Req

	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	var inStream, outStream streamCapability
	if pkt.Flag.Get(codec.FlagStream) {
		switch req.Type {
		case "duplex":
			inStream, outStream = streamCapMultiple, streamCapMultiple
		case "source":
			inStream, outStream = streamCapNone, streamCapMultiple
		case "sink":
			inStream, outStream = streamCapMultiple, streamCapNone
		default:
			return nil, errors.Errorf("unhandled request type: %q", req.Type)
		}
	} else {
		if req.Type == "" {
			req.Type = "async"
		}
		if req.Type != "async" {
			return nil, errors.Errorf("unhandled request type: %q", req.Type)
		}
		inStream, outStream = streamCapNone, streamCapOnce

	}
	req.Stream = newStream(inSrc, r.pkr, pkt.Req, inStream, outStream)
	req.in = inSink

	level.Debug(r.logger).Log("event", "got request", "reqID", req.id, "method", req.Method, "type", req.Type)

	return &req, nil
}

func isTrue(data []byte) bool {
	return len(data) == 4 &&
		data[0] == 't' &&
		data[1] == 'r' &&
		data[2] == 'u' &&
		data[3] == 'e'
}

// fetchRequest returns the request from the reqs map or, if it's not there yet, builds a new one.
func (r *rpc) fetchRequest(ctx context.Context, hdr *codec.Header) (*Request, bool, error) {
	var err error

	r.rLock.Lock()
	defer r.rLock.Unlock()

	// get request from map, otherwise make new one
	req, ok := r.reqs[hdr.Req]
	if !ok {
		req, err = r.ParseRequest(hdr)
		if err != nil {
			return nil, false, errors.Wrap(err, "error parsing request")
		}
		ctx, req.abort = context.WithCancel(ctx)

		r.reqs[hdr.Req] = req
		// TODO:
		// buffer new requests to not mindlessly spawn goroutines
		// and prioritize exisitng requests to unblock the connection time
		// maybe use two maps
		go func() {
			r.root.HandleCall(ctx, req, r)
			level.Debug(r.logger).Log("call", "returned", "method", req.Method, "reqID", req.id)
		}()
	}

	return req, !ok, nil
}

// Server can handle packets to and from a remote party
type Server interface {
	Remote() net.Addr
	Serve(context.Context) error
}

// Serve handles the RPC session
func (r *rpc) Serve(ctx context.Context) (err error) {
	level.Debug(r.logger).Log("event", "serving")
	defer func() {
		cerr := r.pkr.Close()
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			level.Info(r.logger).Log("event", "closed", "handleErr", err, "closeErr", cerr)
		}
	}()

	for {
		var hdr codec.Header

		// read next packet from connection
		doRet := func() bool {
			err = r.pkr.NextHeader(ctx, &hdr)
			if luigi.IsEOS(err) || isAlreadyClosed(err) {
				err = nil
				return true
			}

			r.tLock.Lock()
			defer r.tLock.Unlock()

			if err != nil {
				if r.terminated {
					err = nil
					return true
				}
				err = errors.Wrap(err, "serve failed to read from packer")
				return true
			}

			return false
		}()
		if doRet {
			r.Terminate()
			return
		}

		// error handling and cleanup
		var req *Request
		if hdr.Flag.Get(codec.FlagEndErr) {
			getReq := func(req int32) (*Request, bool) {
				r.rLock.Lock()
				defer r.rLock.Unlock()

				r, ok := r.reqs[req]
				return r, ok
			}

			var ok bool
			if req, ok = getReq(hdr.Req); ok {
				var streamErr error
				req.abort()

				buf := r.bpool.Get()

				err = r.pkr.r.ReadBodyInto(buf, hdr.Len)
				if err != nil {
					return errors.Wrapf(err, "muxrpc: failed to get error body for closing of req: %d (len:%d)", hdr.Req, hdr.Len)
				}

				body := buf.Bytes()

				if !isTrue(body) {
					streamErr, err = parseError(body)
					if err != nil {
						return errors.Wrap(err, "error parsing error packet")
					}
				}
				go func() {
					r.closeStream(req, streamErr)
					r.bpool.Put(buf)
				}()
			} else {
				level.Warn(r.logger).Log("event", "unhandled packet", "reqID", hdr.Req)
			}
			continue
		}

		var isNew bool
		req, isNew, err = r.fetchRequest(ctx, &hdr)
		if err != nil {
			err = errors.Wrap(err, "muxrpc: error getting request")
			return
		}
		if isNew {
			continue
		}

		if req.in == nil { // legacy sink
			err = req.consume(hdr.Len, r.pkr.r.NextBodyReader(hdr.Len))
			if err != nil {
				err = errors.Wrap(err, "muxrpc: error pouring data to handler")
				return
			}
		} else {
			var pkt = new(codec.Packet)

			pkt.Flag = hdr.Flag
			pkt.Req = hdr.Req

			// buf := r.bpool.Get()
			pkt.Body = make([]byte, hdr.Len)

			_, err = io.ReadFull(r.pkr.r.NextBodyReader(hdr.Len), pkt.Body)
			if err != nil {
				return errors.Wrapf(err, "muxrpc: failed to get error body for closing of %d", hdr.Req)
			}

			err = req.in.Pour(ctx, pkt)
			if err != nil {
				err = errors.Wrap(err, "muxrpc: error pouring data to handler")
				return
			}
		}
	}
}

func (r *rpc) closeStream(req *Request, streamErr error) {
	err := req.CloseWithError(streamErr)
	if err != nil {
		level.Warn(r.logger).Log("event", "close stream failed", "reqID", req.id, "method", req.Method.String(), "err", err)
	}

	r.rLock.Lock()
	defer r.rLock.Unlock()
	delete(r.reqs, req.id)
	return
}

func (r *rpc) Remote() net.Addr {
	return r.remote
}

// CallError is returned when a call fails
type CallError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	Stack   string `json:"stack"`
}

func (e *CallError) Error() string {
	return fmt.Sprintf("muxrpc CallError: %s - %s", e.Name, e.Message)
}

func parseError(data []byte) (*CallError, error) {
	var e CallError

	err := json.Unmarshal(data, &e)
	if err != nil {
		return nil, errors.Wrap(err, "error unmarshaling error packet")
	}

	// There are also TypeErrors and numerous other things we might get from this..
	// if e.Name != "Error" {
	// 	return nil, errors.Errorf(`name is not "Error" but %q`, e.Name)
	// }

	return &e, nil
}
