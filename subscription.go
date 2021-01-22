package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// Subscription transport follow Apollo's subscriptions-transport-ws protocol specification
// https://github.com/apollographql/subscriptions-transport-ws/blob/master/PROTOCOL.md

// OperationMessageType defines operation messages for Apollo's GraphQL WebSocket protocol
type OperationMessageType string

const (
	// GqlConnectionInit Client sends this message after plain websocket connection to start the communication with the server
	GqlConnectionInit OperationMessageType = "connection_init"
	// GqlConnectionError The server may responses with this message to the GqlConnectionInit from client, indicates the server rejected the connection.
	GqlConnectionError OperationMessageType = "conn_err"
	// GqlStart Client sends this message to execute GraphQL operation
	GqlStart OperationMessageType = "start"
	// GqlStop Client sends this message in order to stop a running GraphQL operation execution (for example: unsubscribe)
	GqlStop OperationMessageType = "stop"
	// GqlError Server sends this message upon a failing operation, before the GraphQL execution,
	// usually due to GraphQL validation errors (resolver errors are part of GqlData message, and will be added as errors array)
	GqlError OperationMessageType = "error"
	// GqlData The server sends this message to transfer the GraphQL execution result from the server to the client.
	// This message is a response for GqlStart message.
	GqlData OperationMessageType = "data"
	// GqlComplete Server sends this message to indicate that a GraphQL operation is done, and no more data will arrive for the specific operation.
	GqlComplete OperationMessageType = "complete"
	// GqlConnectionKeepAlive Server message that should be sent right after each GqlConnectionAck processed and then periodically to keep the client connection alive.
	// The client starts to consider the keep alive message only upon the first received keep alive message from the server.
	GqlConnectionKeepAlive OperationMessageType = "ka"
	// GqlConnectionAck The server may responses with this message to the GqlConnectionInit from client, indicates the server accepted the connection. May optionally include a payload.
	GqlConnectionAck OperationMessageType = "connection_ack"
	// GqlConnectionTerminate Client sends this message to terminate the connection.
	GqlConnectionTerminate OperationMessageType = "connection_terminate"
	// GqlUnknown Unknown operation type, for logging only
	GqlUnknown OperationMessageType = "unknown"
	// GqlInternal Internal status, for logging only
	GqlInternal OperationMessageType = "internal"
)

// OperationMessage is the message structure that is sent to server
type OperationMessage struct {
	ID      string               `json:"id,omitempty"`
	Type    OperationMessageType `json:"type"`
	Payload json.RawMessage      `json:"payload,omitempty"`
}

func (om OperationMessage) String() string {
	bs, _ := json.Marshal(om)

	return string(bs)
}

// WebsocketConn abstracts WebSocket connecton functions
// ReadJSON and WriteJSON data of a frame from the WebSocket connection.
// Close the WebSocket connection.
type WebsocketConn interface {
	ReadJSON(v interface{}) error
	WriteJSON(v interface{}) error
	Close() error
	// SetReadLimit sets the maximum size in bytes for a message read from the peer. If a
	// message exceeds the limit, the connection sends a close message to the peer
	// and returns ErrReadLimit to the application.
	SetReadLimit(limit int64)
}

type handlerFunc func(message OperationMessage) error
type subscription struct {
	query      string
	variables  map[string]interface{}
	handler    func(message OperationMessage)
	started    Boolean
	restarting Boolean
}

// SubscriptionClient is a GraphQL subscription client.
type SubscriptionClient struct {
	url              string
	conn             WebsocketConn
	connectionParams map[string]interface{}
	context          context.Context
	subscriptions    map[string]*subscription
	cancel           context.CancelFunc
	subscribersMu    sync.Mutex
	timeout          time.Duration
	isRunning        Boolean
	readLimit        int64 // max size of response message. Default 10 MB
	log              func(args ...interface{})
	createConn       func(sc *SubscriptionClient) (WebsocketConn, error)
	retryTimeout     time.Duration
	onConnected      func()
	onDisconnected   func()
	onError          func(sc *SubscriptionClient, err error) error
	errorChan        chan error
	disabledLogTypes []OperationMessageType
}

// NewSubscriptionClient returns new SubscriptionClient
func NewSubscriptionClient(url string) *SubscriptionClient {
	return &SubscriptionClient{
		url:           url,
		timeout:       time.Minute,
		readLimit:     10 * 1024 * 1024, // set default limit 10MB
		subscriptions: make(map[string]*subscription),
		createConn:    newWebsocketConn,
		retryTimeout:  time.Minute,
		errorChan:     make(chan error),
	}
}

// GetURL returns GraphQL server's URL
func (sc *SubscriptionClient) GetURL() string {
	return sc.url
}

// GetContext returns current context of subscription client
func (sc *SubscriptionClient) GetContext() context.Context {
	return sc.context
}

// GetTimeout returns write timeout of websocket client
func (sc *SubscriptionClient) GetTimeout() time.Duration {
	return sc.timeout
}

// WithWebSocket replaces customized websocket client constructor
// In default, subscription client uses https://github.com/nhooyr/websocket
func (sc *SubscriptionClient) WithWebSocket(fn func(sc *SubscriptionClient) (WebsocketConn, error)) *SubscriptionClient {
	sc.createConn = fn
	return sc
}

// WithConnectionParams updates connection params for sending to server through GqlConnectionInit event
// It's usually used for authentication handshake
func (sc *SubscriptionClient) WithConnectionParams(params map[string]interface{}) *SubscriptionClient {
	sc.connectionParams = params
	return sc
}

// WithTimeout updates write timeout of websocket client
func (sc *SubscriptionClient) WithTimeout(timeout time.Duration) *SubscriptionClient {
	sc.timeout = timeout
	return sc
}

// WithRetryTimeout updates reconnecting timeout. When the websocket server was stopped, the client will retry connecting every second until timeout
func (sc *SubscriptionClient) WithRetryTimeout(timeout time.Duration) *SubscriptionClient {
	sc.retryTimeout = timeout
	return sc
}

// WithLog sets loging function to print out received messages. By default, nothing is printed
func (sc *SubscriptionClient) WithLog(logger func(args ...interface{})) *SubscriptionClient {
	sc.log = logger
	return sc
}

// WithoutLogTypes these operation types won't be printed
func (sc *SubscriptionClient) WithoutLogTypes(types ...OperationMessageType) *SubscriptionClient {
	sc.disabledLogTypes = types
	return sc
}

// WithReadLimit set max size of response message
func (sc *SubscriptionClient) WithReadLimit(limit int64) *SubscriptionClient {
	sc.readLimit = limit
	return sc
}

// OnError event is triggered when there is any connection error. This is bottom exception handler level
// If this function is empty, or returns nil, the error is ignored
// If returns error, the websocket connection will be terminated
func (sc *SubscriptionClient) OnError(onError func(sc *SubscriptionClient, err error) error) *SubscriptionClient {
	sc.onError = onError
	return sc
}

// OnConnected event is triggered when the websocket connected to GraphQL server sucessfully
func (sc *SubscriptionClient) OnConnected(fn func()) *SubscriptionClient {
	sc.onConnected = fn
	return sc
}

// OnDisconnected event is triggered when the websocket server was stil down after retry timeout
func (sc *SubscriptionClient) OnDisconnected(fn func()) *SubscriptionClient {
	sc.onDisconnected = fn
	return sc
}

func (sc *SubscriptionClient) setIsRunning(value Boolean) {
	sc.subscribersMu.Lock()
	sc.isRunning = value
	sc.subscribersMu.Unlock()
}

func (sc *SubscriptionClient) init() error {

	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	sc.context = ctx
	sc.cancel = cancel

	for {
		var err error
		var conn WebsocketConn
		// allow custom websocket client
		if sc.conn == nil {
			conn, err = newWebsocketConn(sc)
			if err == nil {
				sc.conn = conn
			}
		}

		if err == nil {
			sc.conn.SetReadLimit(sc.readLimit)
			// send connection init event to the server
			err = sc.sendConnectionInit()
		}

		if err == nil {
			return nil
		}

		if now.Add(sc.retryTimeout).Before(time.Now()) {
			if sc.onDisconnected != nil {
				sc.onDisconnected()
			}
			return err
		}
		sc.printLog(err.Error()+". retry in second....", GqlInternal)
		time.Sleep(time.Second)
	}
}

func (sc *SubscriptionClient) printLog(message interface{}, opType OperationMessageType) {
	if sc.log == nil {
		return
	}
	for _, ty := range sc.disabledLogTypes {
		if ty == opType {
			return
		}
	}

	sc.log(message)
}

func (sc *SubscriptionClient) sendConnectionInit() (err error) {
	var bParams []byte = nil
	if sc.connectionParams != nil {

		bParams, err = json.Marshal(sc.connectionParams)
		if err != nil {
			return
		}
	}

	// send connection_init event to the server
	msg := OperationMessage{
		Type:    GqlConnectionInit,
		Payload: bParams,
	}

	sc.printLog(msg, GqlConnectionInit)
	return sc.conn.WriteJSON(msg)
}

// Subscribe sends start message to server and open a channel to receive data.
// The handler callback function will receive raw message data or error. If the call return error, onError event will be triggered
// The function returns subscription ID and error. You can use subscription ID to unsubscribe the subscription
func (sc *SubscriptionClient) Subscribe(v interface{}, variables map[string]interface{}, handler func(message OperationMessage) error) (string, error) {
	query := constructSubscription(v, variables, "")
	return sc.createSubscription(query, variables, handler)
}

// NamedSubscribe sends start message to server and open a channel to receive data, with operation name
func (sc *SubscriptionClient) NamedSubscribe(name string, v interface{}, variables map[string]interface{}, handler func(message OperationMessage) error) (string, error) {
	query := constructSubscription(v, variables, name)
	return sc.createSubscription(query, variables, handler)
}

// StringSubscribe sends start message to server and open a channel to receive data.
// Takes query parameter as string.
func (sc *SubscriptionClient) StringSubscribe(query string, variables map[string]interface{}, handler func(message OperationMessage) error) (string, error) {
	return sc.createSubscription(query, variables, handler)
}

func (sc *SubscriptionClient) createSubscription(query string, variables map[string]interface{}, handler func(message OperationMessage) error) (string, error) {
	id := uuid.New().String()
	sub := subscription{
		query:     query,
		variables: variables,
		handler:   sc.wrapHandler(handler),
	}

	// if the websocket client is running and the connection is valid, start subscription immediately
	if sc.isRunning && sc.conn != nil {
		if err := sc.startSubscription(id, &sub); err != nil {
			return "", err
		}
	}

	sc.subscribersMu.Lock()
	sc.subscriptions[id] = &sub
	sc.subscribersMu.Unlock()

	return id, nil
}

// Subscribe sends start message to server and open a channel to receive data
func (sc *SubscriptionClient) startSubscription(id string, sub *subscription) error {
	if sub == nil || sub.started {
		return nil
	}

	in := struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables,omitempty"`
	}{
		Query:     sub.query,
		Variables: sub.variables,
	}

	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}

	// send stop message to the server
	msg := OperationMessage{
		ID:      id,
		Type:    GqlStart,
		Payload: payload,
	}

	sc.printLog(msg, GqlStart)
	if err := sc.conn.WriteJSON(msg); err != nil {
		return err
	}

	sub.restarting = false
	sub.started = true
	return nil
}

func (sc *SubscriptionClient) wrapHandler(fn handlerFunc) func(message OperationMessage) {
	return func(message OperationMessage) {
		if errValue := fn(message); errValue != nil {
			sc.errorChan <- errValue
		}
	}
}

// Run start websocket client and subscriptions. If this function is run with goroutine, it can be stopped after closed
func (sc *SubscriptionClient) Run() error {
	if err := sc.init(); err != nil {
		return fmt.Errorf("retry timeout. exiting")
	}

	// lazily start subscriptions
	for k, v := range sc.subscriptions {
		if err := sc.startSubscription(k, v); err != nil {
			sc.Unsubscribe(k)
			return err
		}
	}
	sc.setIsRunning(true)

	for sc.isRunning {
		select {
		case <-sc.context.Done():
			return nil
		case e := <-sc.errorChan:
			if sc.onError != nil {
				if err := sc.onError(sc, e); err != nil {
					return err
				}
			}
		default:

			var message OperationMessage
			if err := sc.conn.ReadJSON(&message); err != nil {
				// manual EOF check
				if err == io.EOF || strings.Contains(err.Error(), "EOF") {
					return sc.Reset()
				}
				closeStatus := websocket.CloseStatus(err)
				if closeStatus == websocket.StatusNormalClosure {
					// close event from websocket client, exiting...
					return nil
				}
				if closeStatus != -1 {
					sc.printLog(fmt.Sprintf("%s. Retry connecting...", err), GqlInternal)
					return sc.Reset()
				}

				if sc.onError != nil {
					if err = sc.onError(sc, err); err != nil {
						return err
					}
				}
				continue
			}

			switch message.Type {
			case GqlError:
				fallthrough
			case GqlData:
				sc.runSubHandler(message)
			case GqlComplete:
				sc.Unsubscribe(message.ID)
			case GqlConnectionAck:
				if sc.onConnected != nil {
					sc.onConnected()
				}
			}
		}
	}

	// if the running status is false, stop retrying
	if !sc.isRunning {
		return nil
	}

	return sc.Reset()
}

func (sc *SubscriptionClient) runSubHandler(message OperationMessage) {
	sub := sc.findSubscription(message.ID)
	if sub == nil {
		return
	}
	go sub.handler(message)
}

// Unsubscribe sends stop message to server and close subscription channel
// The input parameter is subscription ID that is returned from Subscribe function
func (sc *SubscriptionClient) Unsubscribe(id string) error {
	_, ok := sc.subscriptions[id]
	if !ok {
		return fmt.Errorf("subscription id %s doesn't not exist", id)
	}

	err := sc.stopSubscription(id)

	sc.subscribersMu.Lock()
	delete(sc.subscriptions, id)
	sc.subscribersMu.Unlock()
	return err
}

func (sc *SubscriptionClient) stopSubscription(id string) error {
	if sc.conn != nil {
		// send stop message to the server
		msg := OperationMessage{
			ID:   id,
			Type: GqlStop,
		}

		sc.printLog(msg, GqlStop)
		if err := sc.conn.WriteJSON(msg); err != nil {
			return err
		}

	}

	return nil
}

func (sc *SubscriptionClient) findSubscription(ID string) *subscription {
	id, err := uuid.Parse(ID)
	if err != nil {
		return nil
	}
	if subscription, ok := sc.subscriptions[id.String()]; ok {
		return subscription
	}
	return nil
}

func (sc *SubscriptionClient) terminate() error {
	if sc.conn != nil {
		// send terminate message to the server
		msg := OperationMessage{
			Type: GqlConnectionTerminate,
		}

		sc.printLog(msg, GqlConnectionTerminate)
		return sc.conn.WriteJSON(msg)
	}

	return nil
}

// Reset restart websocket connection and subscriptions
func (sc *SubscriptionClient) Reset() error {
	if !sc.isRunning {
		return nil
	}

	for id, sub := range sc.subscriptions {
		_ = sc.stopSubscription(id)
		sub.started = false
		sub.restarting = true
	}

	if sc.conn != nil {
		_ = sc.terminate()
		_ = sc.conn.Close()
		sc.conn = nil
	}
	sc.cancel()

	return sc.Run()
}

// Close closes all subscription channel and websocket as well
func (sc *SubscriptionClient) Close() (err error) {
	sc.setIsRunning(false)
	for id := range sc.subscriptions {
		if err = sc.Unsubscribe(id); err != nil {
			sc.cancel()
			return err
		}
	}
	if sc.conn != nil {
		_ = sc.terminate()
		err = sc.conn.Close()
		sc.conn = nil
	}
	sc.cancel()

	return
}

// default websocket handler implementation using https://github.com/nhooyr/websocket
type websocketHandler struct {
	ctx     context.Context
	timeout time.Duration
	*websocket.Conn
}

func (wh *websocketHandler) WriteJSON(v interface{}) error {
	ctx, cancel := context.WithTimeout(wh.ctx, wh.timeout)
	defer cancel()

	return wsjson.Write(ctx, wh.Conn, v)
}

func (wh *websocketHandler) ReadJSON(v interface{}) error {
	ctx, cancel := context.WithTimeout(wh.ctx, wh.timeout)
	defer cancel()
	return wsjson.Read(ctx, wh.Conn, v)
}

func (wh *websocketHandler) Close() error {
	return wh.Conn.Close(websocket.StatusNormalClosure, "close websocket")
}

func newWebsocketConn(sc *SubscriptionClient) (WebsocketConn, error) {

	options := &websocket.DialOptions{
		Subprotocols: []string{"graphql-ws"},
	}
	c, _, err := websocket.Dial(sc.GetContext(), sc.GetURL(), options)
	if err != nil {
		return nil, err
	}

	return &websocketHandler{
		ctx:     sc.GetContext(),
		Conn:    c,
		timeout: sc.GetTimeout(),
	}, nil
}
