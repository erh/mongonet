package mongonet

import "crypto/tls"
import "fmt"
import "io"
import "net"

import "gopkg.in/mgo.v2/bson"

import "github.com/mongodb/slogger/v2/slogger"

type Proxy struct {
	config   ProxyConfig
	connPool *ConnectionPool

	logger *slogger.Logger
}

type ProxySession struct {
	proxy *Proxy
	conn  net.Conn

	interceptor ProxyInterceptor

	logger *slogger.Logger

	SSLServerName string
}

type MongoError struct {
	err      error
	code     int
	codeName string
}

var NoMongoError MongoError = MongoError{}

func NewMongoError(err error, code int, codeName string) MongoError {
	return MongoError{err, code, codeName}
}

func (me MongoError) ToBSON() bson.D {
	if me == NoMongoError {
		return bson.D{{"ok", 1}}
	}

	doc := bson.D{{"ok", 0}}

	if me.err != nil {
		doc = append(doc, bson.DocElem{"errmsg", me.err.Error()})
	}

	doc = append(doc,
		bson.DocElem{"code", me.code},
		bson.DocElem{"codeName", me.codeName})

	return doc
}

type ResponseInterceptor interface {
	InterceptMongoToClient(m Message) (Message, error)
}

type ProxyInterceptor interface {
	InterceptClientToMongo(m Message) (Message, ResponseInterceptor, MongoError)
	Close()
	TrackRequest(MessageHeader)
	TrackResponse(MessageHeader)
}

type ProxyInterceptorFactory interface {
	// This has to be thread safe, will be called from many clients
	NewInterceptor(ps *ProxySession) (ProxyInterceptor, error)
}

// -----

func (ps *ProxySession) RemoteAddr() net.Addr {
	return ps.conn.RemoteAddr()
}

func (ps *ProxySession) GetLogger() *slogger.Logger {
	return ps.logger
}

func (ps *ProxySession) ServerPort() int {
	return ps.proxy.config.BindPort
}

func (ps *ProxySession) Stats() bson.D {
	return bson.D{
		{"connectionPool", bson.D{
			{"totalCreated", ps.proxy.connPool.totalCreated},
		},
		},
	}
}

func (ps *ProxySession) RespondToCommand(clientMessage Message, doc SimpleBSON) error {
	if clientMessage.Header().OpCode == OP_QUERY {
		rm := &ReplyMessage{
			MessageHeader{
				0,
				17, // TODO
				clientMessage.Header().RequestID,
				OP_REPLY},
			0, // flags - error bit
			0, // cursor id
			0, // StartingFrom
			1, // NumberReturned
			[]SimpleBSON{doc},
		}
		return SendMessage(rm, ps.conn)
	} else if clientMessage.Header().OpCode == OP_COMMAND {
		rm := &CommandReplyMessage{
			MessageHeader{
				0,
				17, // TODO
				clientMessage.Header().RequestID,
				OP_COMMAND_REPLY},
			doc,
			SimpleBSONEmpty(),
			[]SimpleBSON{},
		}
		return SendMessage(rm, ps.conn)
	} else {
		panic("impossible")
	}

}

func (ps *ProxySession) respondWithError(clientMessage Message, mongoErr MongoError) error {
	ps.logger.Logf(slogger.INFO, "respondWithError %v", mongoErr)

	switch clientMessage.Header().OpCode {
	case OP_QUERY, OP_GET_MORE:
		doc, myErr := SimpleBSONConvert(mongoErr.ToBSON())
		if myErr != nil {
			return myErr
		}

		rm := &ReplyMessage{
			MessageHeader{
				0,
				17, // TODO
				clientMessage.Header().RequestID,
				OP_REPLY},
			2, // flags - error bit
			0, // cursor id
			0, // StartingFrom
			1, // NumberReturned
			[]SimpleBSON{doc},
		}
		return SendMessage(rm, ps.conn)
	case OP_COMMAND:
		doc, myErr := SimpleBSONConvert(mongoErr.ToBSON())
		if myErr != nil {
			return myErr
		}

		rm := &CommandReplyMessage{
			MessageHeader{
				0,
				17, // TODO
				clientMessage.Header().RequestID,
				OP_COMMAND_REPLY},
			doc,
			SimpleBSONEmpty(),
			[]SimpleBSON{},
		}
		return SendMessage(rm, ps.conn)
	default:
		panic("impossible")
	}

}

func (ps *ProxySession) doLoop(pooledConn *PooledConnection) (*PooledConnection, error) {
	m, err := ReadMessage(ps.conn)
	if err != nil {
		if err == io.EOF {
			return pooledConn, err
		}
		return pooledConn, NewStackErrorf("got error reading from client: %s", err)
	}

	var respInter ResponseInterceptor
	if ps.interceptor != nil {
		var mongoErr MongoError
		ps.interceptor.TrackRequest(m.Header())

		m, respInter, mongoErr = ps.interceptor.InterceptClientToMongo(m)
		if mongoErr != NoMongoError {
			if m == nil {
				if pooledConn != nil {
					pooledConn.Close()
				}
				return nil, mongoErr.err
			}
			if !m.HasResponse() {
				// we can't respond, so we just fail
				return pooledConn, mongoErr.err
			}
			err = ps.respondWithError(m, mongoErr)
			if err != nil {
				return pooledConn, NewStackErrorf("couldn't send error response to client %s", err)
			}
			return pooledConn, nil
		}
		if m == nil {
			// already responded
			return pooledConn, nil
		}
	}

	if pooledConn == nil {
		pooledConn, err = ps.proxy.connPool.Get()
		if err != nil {
			return nil, NewStackErrorf("cannot get connection to mongo %s", err)
		}
	}

	if pooledConn.closed {
		panic("oh no!")
	}
	mongoConn := pooledConn.conn

	err = SendMessage(m, mongoConn)
	if err != nil {
		return nil, NewStackErrorf("error writing to mongo: %s", err)
	}

	if !m.HasResponse() {
		return pooledConn, nil
	}

	defer pooledConn.Close()

	inExhaustMode :=
		m.Header().OpCode == OP_QUERY &&
			m.(*QueryMessage).Flags&(1<<6) != 0

	for {
		resp, err := ReadMessage(mongoConn)
		if err != nil {
			pooledConn.bad = true
			return nil, NewStackErrorf("got error reading response from mongo %s", err)
		}

		if respInter != nil {
			resp, err = respInter.InterceptMongoToClient(resp)
			if err != nil {
				return nil, NewStackErrorf("error intercepting message %s", err)
			}
		}

		err = SendMessage(resp, ps.conn)
		if err != nil {
			return nil, NewStackErrorf("got error sending response to client %s", err)
		}

		if ps.interceptor != nil {
			ps.interceptor.TrackResponse(resp.Header())
		}

		if !inExhaustMode {
			return nil, nil
		}

		if resp.(*ReplyMessage).CursorId == 0 {
			return nil, nil
		}
	}
}

func (ps *ProxySession) Run() {
	var err error
	defer ps.conn.Close()

	switch c := ps.conn.(type) {
	case *tls.Conn:
		// we do this here so that we can get the SNI server name
		err = c.Handshake()
		if err != nil {
			ps.logger.Logf(slogger.ERROR, "error doing tls handshake %s", err)
			return
		}
		ps.SSLServerName = c.ConnectionState().ServerName
	}

	ps.logger.Logf(slogger.INFO, "new connection SSLServerName [%s]", ps.SSLServerName)

	if ps.proxy.config.InterceptorFactory != nil {
		ps.interceptor, err = ps.proxy.config.InterceptorFactory.NewInterceptor(ps)
		if err != nil {
			ps.logger.Logf(slogger.INFO, "error creating new interceptor because: %s", err)
			return
		}
		defer ps.interceptor.Close()
	}

	defer ps.logger.Logf(slogger.INFO, "socket closed")

	var pooledConn *PooledConnection = nil

	for {
		pooledConn, err = ps.doLoop(pooledConn)
		if err != nil {
			if pooledConn != nil {
				pooledConn.Close()
			}
			if err != io.EOF {
				ps.logger.Logf(slogger.WARN, "error doing loop: %s", err)
			}
			return
		}
	}

	if pooledConn != nil {
		pooledConn.Close()
	}

}

// -------

func NewProxy(pc ProxyConfig) Proxy {
	p := Proxy{pc, NewConnectionPool(pc.MongoAddress(), pc.MongoSSL, pc.MongoRootCAs, pc.MongoSSLSkipVerify, pc.ConnectionPoolHook), nil}

	p.logger = p.NewLogger("proxy")

	return p
}

func (p *Proxy) NewLogger(prefix string) *slogger.Logger {
	filters := []slogger.TurboFilter{slogger.TurboLevelFilter(p.config.LogLevel)}

	appenders := p.config.Appenders
	if appenders == nil {
		appenders = []slogger.Appender{slogger.StdOutAppender()}
	}

	return &slogger.Logger{prefix, appenders, 0, filters}
}

func (p *Proxy) Run() error {

	bindTo := fmt.Sprintf("%s:%d", p.config.BindHost, p.config.BindPort)
	p.logger.Logf(slogger.WARN, "listening on %s", bindTo)

	var err error
	var ln net.Listener

	if p.config.UseSSL {
		if len(p.config.SSLKeys) == 0 {
			return fmt.Errorf("no ssl keys configured")
		}

		certs := []tls.Certificate{}
		for _, pair := range p.config.SSLKeys {
			cer, err := tls.LoadX509KeyPair(pair.CertFile, pair.KeyFile)
			if err != nil {
				return fmt.Errorf("cannot LoadX509KeyPair from %s %s %s", pair.CertFile, pair.KeyFile, err)
			}
			certs = append(certs, cer)
		}

		config := &tls.Config{Certificates: certs}

		config.BuildNameToCertificate()

		ln, err = tls.Listen("tcp", bindTo, config)
		if err != nil {
			return fmt.Errorf("cannot start listen tls in proxy: %s", err)
		}

	} else {
		ln, err = net.Listen("tcp", bindTo)
		if err != nil {
			return NewStackErrorf("cannot start listening in proxy: %s", err)
		}
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return NewStackErrorf("could not accept in proxy: %s", err)
		}

		c := &ProxySession{p, conn, nil, p.NewLogger(fmt.Sprintf("ProxySession %s", conn.RemoteAddr())), ""}
		go c.Run()
	}

}
