package muxrpc // import "cryptoscope.co/go/muxrpc"

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/pkg/errors"

	"cryptoscope.co/go/luigi"
	"cryptoscope.co/go/muxrpc/codec"
)

type rpc struct {
	l sync.Mutex

	// pkr is the Sink and Source of the network connection
	pkr Packer

	// reqs is the map we keep, tracking all requests
	reqs    map[int32]*Request
	highest int32

	root Handler

	// terminated indicates that the rpc session is being terminated
	terminated bool
}

type Handler interface {
	OnCall(ctx context.Context, req *Request)
	OnConnect(ctx context.Context, e Endpoint)
}

const bufSize = 5
const rxTimeout time.Duration = time.Millisecond

func Handle(pkr Packer, handler Handler) Endpoint {
	r := &rpc{
		pkr:  pkr,
		reqs: make(map[int32]*Request),
		root: handler,
	}

	go handler.OnConnect(context.Background(), r)
	return r
}

// Async does an aync call to the endpoint.
func (r *rpc) Async(ctx context.Context, tipe interface{}, method []string, args ...interface{}) (interface{}, error) {
	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	req := &Request{
		Type:   "async",
		Stream: NewStream(inSrc, r.pkr, 0),
		in:     inSink,

		Method: method,
		Args:   args,

		tipe: tipe,
	}

	err := r.Do(ctx, req)
	if err != nil {
		return nil, errors.Wrap(err, "error sending request")
	}

	v, err := req.Stream.Next(ctx)
	return v, errors.Wrap(err, "error reading response from request source")
}

func (r *rpc) Source(ctx context.Context, tipe interface{}, method []string, args ...interface{}) (luigi.Source, error) {
	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	req := &Request{
		Type:   "source",
		Stream: NewStream(inSrc, r.pkr, 0),
		in:     inSink,

		Method: method,
		Args:   args,

		tipe: tipe,
	}

	err := r.Do(ctx, req)
	if err != nil {
		return nil, errors.Wrap(err, "error sending request")
	}

	return req.Stream, nil
}

func (r *rpc) Sink(ctx context.Context, method []string, args ...interface{}) (luigi.Sink, error) {
	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	req := &Request{
		Type:   "sink",
		Stream: NewStream(inSrc, r.pkr, 0),
		in:     inSink,

		Method: method,
		Args:   args,
	}

	err := r.Do(ctx, req)
	if err != nil {
		return nil, errors.Wrap(err, "error sending request")
	}

	return req.Stream, nil
}

func (r *rpc) Duplex(ctx context.Context, method []string, args ...interface{}) (luigi.Source, luigi.Sink, error) {
	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))

	req := &Request{
		Type:   "duplex",
		Stream: NewStream(inSrc, r.pkr, 0),
		in:     inSink,

		Method: method,
		Args:   args,
	}

	err := r.Do(ctx, req)
	if err != nil {
		return nil, nil, errors.Wrap(err, "error sending request")
	}

	return req.Stream, req.Stream, nil
}

func (r *rpc) Terminate() error {
	r.l.Lock()
	defer r.l.Unlock()

	r.terminated = true
	return r.pkr.Close()
}

var trueBytes = []byte{'t', 'r', 'u', 'e'}

func buildEndPacket(req int32) *codec.Packet {
	return &codec.Packet{
		Req:  req,
		Flag: codec.FlagJSON | codec.FlagEndErr | codec.FlagStream,
		Body: trueBytes,
	}
}

func (r *rpc) finish(ctx context.Context, req int32) error {
	delete(r.reqs, req)

	err := r.pkr.Pour(ctx, buildEndPacket(req))
	return errors.Wrap(err, "error pouring done message")
}

func (r *rpc) Do(ctx context.Context, req *Request) error {
	var (
		pkt codec.Packet
		err error
	)

	func() {
		r.l.Lock()
		defer r.l.Unlock()

		pkt.Flag = pkt.Flag.Set(codec.FlagJSON)
		pkt.Flag = pkt.Flag.Set(req.Type.Flags())

		pkt.Body, err = json.Marshal(req)

		pkt.Req = r.highest + 1
		r.highest = pkt.Req
		r.reqs[pkt.Req] = req
		req.Stream.WithReq(pkt.Req)

		req.pkt = &pkt
	}()
	if err != nil {
		return err
	}

	return r.pkr.Pour(ctx, &pkt)
}

func (r *rpc) ParseRequest(pkt *codec.Packet) (*Request, error) {
	var req Request

	if !pkt.Flag.Get(codec.FlagJSON) {
		return nil, errors.New("expected JSON flag")
	}

	if pkt.Req >= 0 {
		// request numbers should have been inverted by now
		return nil, errors.New("expected negative request id")
	}

	err := json.Unmarshal(pkt.Body, &req)
	if err != nil {
		return nil, errors.Wrap(err, "error decoding packet")
	}

	req.pkt = pkt

	inSrc, inSink := luigi.NewPipe(luigi.WithBuffer(bufSize))
	req.Stream = NewStream(inSrc, r.pkr, pkt.Req)
	req.in = inSink

	return &req, nil
}

func isTrue(data []byte) bool {
	return len(data) == 4 &&
		data[0] == 't' &&
		data[1] == 'r' &&
		data[2] == 'u' &&
		data[3] == 'e'
}

func (r *rpc) fetchRequest(ctx context.Context, pkt *codec.Packet) (*Request, bool, error) {
	var err error

	// get request from map, otherwise make new one
	req, ok := r.reqs[pkt.Req]
	if !ok {
		req, err = r.ParseRequest(pkt)
		fmt.Printf("ParseRequest returned %+v, %+v\n", req, err)
		if err != nil {
			return nil, false, errors.Wrap(err, "error parsing request")
		}
		r.reqs[pkt.Req] = req

		go r.root.OnCall(ctx, req)
	}

	return req, !ok, nil
}

func (r *rpc) Serve(ctx context.Context) (err error) {
	/*
		defer func() {
			fmt.Printf("Serve returns with err=%q\n", err)

			fmt.Println("finishing all connections - taking lock")
			r.l.Lock()
			defer r.l.Unlock()
			fmt.Println("got lock")

			for req := range r.reqs {
				r.finish(ctx, req)
			}

			//fmt.Println("closing")
			//r.pkr.Close()
		}()
	*/

	for {
		var vpkt interface{}
		// read next packet from connection
		doRet := func() bool {
			vpkt, err = r.pkr.Next(ctx)

			r.l.Lock()
			defer r.l.Unlock()

			if luigi.IsEOS(err) {
				err = nil
				return true
			}
			if err != nil {
				if r.terminated {
					err = nil
					return true
				}
				err = errors.Wrap(err, "error reading from packer source")
				return true
			}

			return false
		}()
		if doRet {
			return err
		}

		pkt := vpkt.(*codec.Packet)

		if pkt.Flag.Get(codec.FlagEndErr) {
			if req, ok := r.reqs[pkt.Req]; ok {
				req.Stream.Close()
				delete(r.reqs, pkt.Req)
			}

			continue
		}

		req, isNew, err := r.fetchRequest(ctx, pkt)
		if err != nil {
			return errors.Wrap(err, "error getting request")
		}
		if isNew {
			continue
		}

		// is this packet ending a stream?
		if pkt.Flag.Get(codec.FlagEndErr) {
			delete(r.reqs, pkt.Req)

			// TODO make type RPCError and return it as error
			if !isTrue(pkt.Body) {
				fmt.Printf("not true: %q\n", pkt.Body)
				err = req.in.Pour(ctx, []byte(pkt.Body))
				if err != nil {
					return errors.Wrap(err, "error writing to pipe sink")
				}
			}

			if req.in != nil {
				err = req.in.Close()
				if err != nil {
					return errors.Wrap(err, "error closing pipe sink")
				}
			}

			select {
			case <-req.Stream.(*stream).closeCh:
			default:
				//pkt.Body = []byte{'t', 'r', 'u', 'e'}
				//pkt.Req = -pkt.Req
				err = r.pkr.Pour(ctx, buildEndPacket(pkt.Req))
				if err != nil {
					return errors.Wrap(err, "error pouring end reply to packer")
				}
			}

			continue
		}

		/*
		   var v interface{}

		   if pkt.Flag.Get(codec.FlagJSON) {
		     if req.tipe != nil {
		       var isPtr bool

		       t := reflect.TypeOf(req.tipe)
		       if t.Kind() == reflect.Ptr {
		         isPtr = true
		         t = t.Elem()
		       }

		       v = reflect.New(t).Interface()
		       err := json.Unmarshal(pkt.Body, &v)
		       if err != nil {
		         return errors.Wrap(err, "error unmarshaling json")
		       }

		       if !isPtr {
		         v = reflect.ValueOf(v).Elem().Interface()
		       }
		     }
		   } else if pkt.Flag.Get(codec.FlagString) {
		     v = string(pkt.Body)
		   } else {
		     v = pkt.Body
		   }
		*/

		// localize defer
		err = func() error {
			// pour may block so we need to time out.
			// note that you can use buffers make this less probable
			ctx, cancel := context.WithTimeout(ctx, rxTimeout)
			defer cancel()

			//err := req.in.Pour(ctx, v)
			err := req.in.Pour(ctx, pkt)
			fmt.Println("poured", pkt, "- err:", err)
			return errors.Wrap(err, "error pouring data to handler")
		}()
		if err != nil {
			return err
		}
	}
}
