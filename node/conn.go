package node

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skycoin/skycoin/src/cipher"

	"github.com/skycoin/net/factory"

	"github.com/skycoin/cxo/data"
	"github.com/skycoin/cxo/node/msg"
	"github.com/skycoin/cxo/skyobject/registry"
)

type fillSent struct {
	f *filler
	s *sender
}

// feed of a connection
type connFeed struct {
	sync.Mutex
	heads map[uint64]fillSent
}

// A Conn represent connection of the Node
type Conn struct {
	*factory.Connection

	// lock
	mx sync.Mutex

	// is incoming or not
	incoming bool

	// is tcp or udp
	tcp bool

	// back reference
	n *Node

	// peer id
	peerID NodeID

	// feeds this connection share with peer
	feeds map[cipher.PubKey]*connFeed

	// amount of Root obejcts currently
	// filling by the Conn
	fillingRoots int

	// messege seq number (for request-response)
	seq uint32

	// requests
	reqs map[uint32]chan<- msg.Msg

	// stat

	// TODO (kostyarin): RPS, WPS

	// collecting

	// rank of the connection; every conneciton has
	// its rank (filling rank), the rank is ...
	// _ or not to do _

	// channels (from factory.Connection)
	sendq  chan<- []byte
	closeq <-chan struct{}

	// wait for receiving loop
	await sync.WaitGroup

	// close once
	closeo sync.Once
}

func (n *Node) newConnection(
	fc *factory.Connection,
	isIncoming bool,
	isTCP bool,
) (
	c *Conn,
) {

	c = new(Conn)

	c.Connection = fc
	c.incoming = isIncoming
	c.tcp = isTCP

	c.n = n

	c.feeds = make(map[cipher.PubKey]*connFeed)
	c.reqs = make(map[uint32]chan<- msg.Msg)

	c.sendq = fc.GetChanOut()
	c.closeq = make(chan struct{})

	n.addPendingConn(c)

	//
	// the next step is c.handshake() and c.run()
	//

	return
}

// start handling
func (c *Conn) run() {
	c.await.Add(1)
	go c.receiving()
}

func (c *Conn) decodeRaw(raw []byte) (seq, rseq uint32, m msg.Msg, err error) {

	if len(raw) < 9 {
		err = errors.New("invlaid messege received: too short")
		return
	}

	seq = binary.LittleEndian.Uint32(raw)
	raw = raw[4:]

	rseq = binary.LittleEndian.Uint32(raw)
	raw = raw[4:]

	m, err = msg.Decode(raw)
	return
}

//
// info
//

// IsTCP returns true if this conenctions
// is tcp connection
func (c *Conn) IsTCP() (tcp bool) {
	return c.tcp
}

// IsUDP retursn true if this conenctions
// si udp conenction
func (c *Conn) IsUDP() (udp bool) {
	return c.tcp == false
}

// PeerID is ID of remote peer that used
// for internals and unique
func (c *Conn) PeerID() (id NodeID) {
	return c.peerID
}

// IsIncoming returns true if this Conn is
// incoming and accepted by listener
func (c *Conn) IsIncomig() (ok bool) {
	return c.incoming
}

// IsOutgoing is inverse of the IsIncoming
func (c *Conn) IsOutgoing() (ok bool) {
	return c.incoming == false
}

// Node returns related Node
func (c *Conn) Node() (node *Node) {
	return c.n
}

// Address returns remote address
// represetned as string
func (c *Conn) Address() (address string) {
	return c.GetRemoteAddr().String()
}

// Feeds returns list of feeds this connection
// share with peer
func (c *Conn) Feeds() (feeds []cipher.PubKey) {
	c.mx.Lock()
	defer c.mx.Unlock()

	feeds = make([]cipher.PubKey, 0, len(c.feeds))

	for pk := range c.feeds {
		feeds = append(feeds, pk)
	}

	return

}

func connString(isIncoming, isTCP bool, addr string) (s string) {

	if isIncoming == true {
		s = "-> "
	} else {
		s = "<- "
	}

	if isTCP == true {
		s += "tcp://"
	} else {
		s += "udp://"
	}

	return s + addr
}

// String returns string "-> network://remote_address"
// for example: "-> tcp://127.0.0.1:8887". Where the
// arrow is "->" for incoming connections and is "<-"
// for outgoing
func (c *Conn) String() (s string) {
	return connString(c.incoming, c.tcp, c.Address())
}

//
// requests
//

// RemoteFeeds requests list of feeds that remote peer share.
// It's possible if the remote peer is public server, otherwise
// it returns "not a public server" error. The request has
// timeout configured by Config
func (c *Conn) RemoteFeeds() (feeds []cipher.PubKey, err error) {

	var reply msg.Msg

	if reply, err = c.sendRequest(&msg.RqList{}); err != nil {
		return
	}

	switch x := reply.(type) {

	case *msg.List:

		feeds = x.List

	case *msg.Err:

		err = errors.New(x.Err)

	default:

		err = fmt.Errorf("invalid response type %T", reply)

	}

	return
}

// Subscribe to gievn feed of remote peer. The Subscribe adds
// feed to the Node if the Node doesn't have the feed calling
// the (*Node).Share method. The feed will not be removed from
// the Node
func (c *Conn) Subscribe(feed cipher.PubKey) (err error) {

	c.mx.Lock()
	defer c.mx.Unlock()

	// already subscribed ?

	if _, ok := c.feeds[feed]; ok == true {
		return
	}

	// add the feed to node

	if err = c.n.Share(feed); err != nil {
		return
	}

	var reply msg.Msg

	if reply, err = c.sendRequest(&msg.Sub{Feed: feed}); err != nil {
		return
	}

	switch x := reply.(type) {

	case *msg.Ok:

		// success

	case *msg.Err:

		err = errors.New(x.Err)

	default:

		err = fmt.Errorf("invalid response type %T", reply)

	}

	if err != nil {
		reutrn
	}

	c.feeds[feed] = new(connFeed)

	return
}

// Unsubscribe from given feed of remote peer
func (c *Conn) Unsubscribe(feed cipher.PubKey) {

	c.mx.Lock()
	defer c.mx.Unlock()

	var cf, ok = c.feeds[feed]

	// not subscribed

	if ok == false {
		return
	}

	cf.Lock()
	defer cf.Unlock()

	cf.unholdSent(c.n)
	delete(c.feeds, cf)

	c.sendMsg(c.nextSeq(), 0, &msg.Unsub{
		Feed: feed,
	})

	return

}

//
// terminate
//

// Close the Conn
func (c *Conn) Close() (err error) {

	c.closeo.Do(func() {

		// TODO

		c.Connection.Close()
	})

	return
}

func (c *Conn) nextSeq() uint32 {
	return atomic.AddUint32(&c.seq, 1)
}

func (c *Conn) encodeMsg(seq, rseq uint32, m msg.Msg) (raw []byte) {

	var em = m.Encode()

	raw = make([]byte, 8, 8+len(em))

	binary.LittleEndian.PutUint32(raw, seq)
	binary.LittleEndian.PutUint32(raw[:4], rseq)

	raw = append(raw, em...)

	return

}

func (c *Conn) sendMsg(seq, rseq uint32, m msg.Msg) {
	c.sendRaw(c.encodeMsg(seq, rseq, m))
}

func (c *Conn) sendRaw(raw []byte) {

	select {
	case c.sendq <- raw:
	case <-c.closeq:
	}

}

func (c *Conn) closeWithError(err error) {

	// TODO

}

func (c *Conn) fatality(args ...interface{}) {

	var err = errors.New(fmt.Sprint(args...))

	c.n.Print("[ERR] ", err)
	c.closeWithError(err)
}

func (c *Conn) receiving() {

	defer c.await.Done()

	var (
		receiveq = c.GetChanIn()
		closeq   = c.closeq

		seq, rseq uint32
		m         msg.Msg
		err       error

		raw []byte
		ok  bool
	)

	for {

		select {

		case raw, ok = <-receiveq:

			if ok == false {
				return
			}

			// [ 4 seq ][ 4 rseq ][ 1 msg type ]

			if len(raw) < 9 {
				c.fatality("invalid messege received: samll size")
				return
			}

			// seq of the Msg
			seq = binary.LittleEndian.Uint32(raw)
			raw = raw[4:]

			// response for a seq or zero
			rseq = binary.LittleEndian.Uint32(raw)
			raw = raw[4:]

			if m, err = msg.Decode(raw); err != nil {
				c.fatality("can't decode received messege: ", err)
				return
			}

			// the messege can be a response for a request
			if rq, ok := c.isResponse(rseq); ok == true {
				rq <- m
				continue
			}

			if err = c.handle(seq, m); err != nil {
				c.fatality("error handling messege: ", err)
				return
			}

		case <-closeq:
			return

		}

	}

}

func (c *Conn) isResponse(rseq uint32) (rq chan<- msg.Msg, ok bool) {
	c.mx.Lock()
	defer c.mx.Unlock()

	rq, ok = c.reqs[rseq]
	return
}

func (c *Conn) addRequest(seq uint32, rq chan<- msg.Msg) {
	c.mx.Lock()
	defer c.mx.Unlock()

	c.reqs[seq] = rq
}

func (c *Conn) delRequest(seq uint32) {
	c.mx.Lock()
	defer c.mx.Unlock()

	delete(c.reqs, seq)
}

func (c *Conn) sendRequest(m msg.Msg) (reply msg.Msg, err error) {

	var (
		tr *time.Timer
		tc <-chan time.Time
	)

	if rt := c.n.config.ResponseTimeout; rt > 0 {
		tr = time.NewTimer(rt)
		tc = tr.C

		defer tr.Stop()
	}

	var (
		rq  = make(chan msg.Msg)
		seq = c.nextSeq()
	)

	c.addRequest(seq, rq)
	defer c.delRequest(seq)

	c.sendMsg(seq, 0, m)

	select {
	case rq <- reply:

		return

	case <-tc:

		return nil, ErrTimeout

	case <-c.closeq:

		return nil, ErrClosed
	}

}

func (c *Conn) sendErr(rseq uint32, err error) {
	c.sendMsg(c.nextSeq(), rseq, &msg.Err{err.Error()})
}

func (c *Conn) sendOk(rseq uint32) {
	c.sendMsg(c.nextSeq(), rseq, &msg.Ok{})
}

// handle messeges except responses and handshakes
func (c *Conn) handle(seq uint32, m msg.Msg) (err error) {

	switch x := m.(type) {

	// subscriptions

	case *msg.Sub: // <- Sub (feed)

		return c.handleSub(seq, x)

	case *msg.Unsub: // <- Unsub (feed)

		return c.handleUnsub(seq, x)

	// public server features

	case *msg.RqList: // <- RqList ()

		return c.handleRqList(seq, x)

	// the *List is response and handled outside the handle()

	// root (push and done)

	case *msg.Root: // <- Root (feed, nonce, seq, sig, val)

		return c.handleRoot(x)

	case *msg.RootDone: // -> RD   (feed, nonce, seq)

		return c.handleRootDone(x)

	case *msg.RootErr: // RE -> (feed, nonce, seq, reason)

		return c.handleRootErr(x)

	// obejcts

	case *msg.RqObject: // <- RqO (key, prefetch)

		return c.handleRqObject(x)

	case *msg.Object: // -> O   (val, vals)

		return c.handleObject(x)

	// preview

	case *msg.RqPreview: // -> RqPreview (feed)

		return c.handleRqPreview(seq, x)

	default:

		return fmt.Errorf("invalid messege type %T", m)

	}

}

// subscribe (with reply)
func (c *Conn) handleSub(seq uint32, sub *msg.Sub) (_ error) {

	var err error // not fatal error

	// don't allow blank

	if sub.Feed == (cipher.PubKey{}) {

		err = errors.New("blank public key")
		c.sendErr(seq, err)

		return
	}

	// already subscribed

	c.mx.Lock()
	defer c.mx.Unlock()

	if _, ok = c.feeds[sub.Feed]; ok == true {
		c.sendOk(seq)
		return
	}

	// callback
	var reject = c.n.onSubscribeRemote(c, feed)

	// reject subscription by callback
	if reject != nil {
		c.sendErr(seq, reject)
		return
	}

	// has feed ?

	c.n.mx.Lock()
	defer c.n.mx.Unlock()

	var cs, ok = c.n.fc[sub.Feed]

	if ok == false {

		err = errors.New("not share the feed")
		c.sendErr(seq, err)

		return
	}

	// ok

	c.n.addConnFeed(c, sub.Feed)

	c.feeds[sub.Feed] = new(connFeed)

	c.sendOk(seq)

	return
}

// unsubscribe (no reply)
func (c *Conn) handleUnsub(seq uint32, unsub *msg.Unsub) (_ error) {

	var err error // not fatal error

	if unsub.Feed == (cipher.PubKey{}) {
		//
		return
	}

	c.mx.Lock()
	defer c.mx.Unlock()

	// don't have the feed

	var cf, ok = c.feeds[unsub.Feed]

	if ok == false {
		return // not subscribed (do nothing)
	}

	// TODO: terminate subscription

	return
}

// request list of feeds
func (c *Conn) handleRqList(seq uint32, rq *msg.RqList) (_ error) {

	if c.n.config.Public == false {
		c.sendErr(seq, ErrNotPublic)
		return
	}

	c.sendMsg(c.nextSeq(), seq, &msg.List{
		Feeds: c.n.Feeds(),
	})

	return
}

// got Root (preview Root objects are handled by request-responnse, not here)
func (c *Conn) handleRoot(root *msg.Root) (_ error) {

	c.mx.Lock()
	defer c.mx.Unlock()

	// do we share the feed

	var cf, ok = c.feeds[root.Feed]

	if ok == false {
		return // not subscribed to the feed
	}

	var (
		r   *registry.Root //
		err error          // not fatal
	)

	if r, err = c.n.c.ReceivedRoot(root.Sig, root.Value); err != nil {
		c.n.Printf("[ERR] [%s] received Root error: %s", c.String(), err)
		return // keep connection ?
	}

	// does nothing, because the Node already have this Root
	if r.IsFull == true {
		return
	}

	// so, let's check seq number, may be the Root is too old
	var last *registry.Root
	if last, err = c.n.c.LastRoot(r.Pub, r.Nonce); err != nil {

		if err != data.ErrNotFound {

			c.n.Printf("[ERR] [%s] %s, can't get last root",
				c.String(), r.Short())

			return

		}

	}

	// the received Root is older then we have
	if last != nil && r.Seq <= last.Seq {
		return
	}

	// if the last is nil, then DB doesn't have any Root of the feed-head

	// let's fill the Root, checking callback first

	c.addToFiller(cf, r)

	return
}

func (c *Conn) handleRootDone(seq uint32, sub *msg.RootDone) (err error) {

	//

	return
}

func (c *Conn) handleRootErr(seq uint32, sub *msg.RootDone) (err error) {

	//

	return
}

func (c *Conn) handleRqObject(seq uint32, rq *msg.RqObject) (_ error) {

	var (
		object   []byte
		prefetch []cipher.SHA256

		err error
	)

	if val, _, err = c.n.c.Get(rq.Key, 0); err != nil {

		if err != data.ErrNotFound {
			c.n.Fatal("[ERR] CXDS DB failure:", err)
			return
		}

		// TODO (add to waiting to push)

		return // not found
	}

	return
}

func (c *Conn) handleObject(seq uint32, sub *msg.Object) (err error) {

	//

	return
}

func (c *Conn) handleRqPreview(seq uint32, sub *msg.RqPreview) (err error) {

	//

	return
}
