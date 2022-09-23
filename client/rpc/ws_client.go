package rpc

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync/atomic"

	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"

	"github.com/tendermint/go-amino"
	libbytes "github.com/tendermint/tendermint/libs/bytes"
	"github.com/tendermint/tendermint/libs/log"
	tmpubsub "github.com/tendermint/tendermint/libs/pubsub"
	tmrand "github.com/tendermint/tendermint/libs/rand"
	libservice "github.com/tendermint/tendermint/libs/service"
	"github.com/tendermint/tendermint/rpc/client"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	rpctypes "github.com/tendermint/tendermint/rpc/jsonrpc/types"
	"github.com/tendermint/tendermint/types"

	"github.com/shapeshift/bnb-chain-go-sdk/common/uuid"
	"github.com/shapeshift/bnb-chain-go-sdk/types/tx"
)

const (
	defaultMaxReconnectAttempts = 25
	defaultWriteWait            = 500 * time.Millisecond
	defaultReadWait             = 0
	defaultPingPeriod           = 0

	protoHTTP  = "http"
	protoHTTPS = "https"
	protoWSS   = "wss"
	protoWS    = "ws"
	protoTCP   = "tcp"
)

//-----------------------------------------------------------------------------
// WSEvents

var errNotRunning = errors.New("client is not running. Use .Start() method to start")

// WSEvents is a wrapper around WSClient, which implements EventsClient.
type WSEvents struct {
	libservice.BaseService
	cdc      *amino.Codec
	remote   string
	endpoint string
	ws       *WSClient

	responseChanMap sync.Map

	mtx           sync.RWMutex
	subscriptions map[string]chan ctypes.ResultEvent // query -> chan

	timeout time.Duration
}

func newWSEvents(cdc *amino.Codec, remote, endpoint string) *WSEvents {
	w := &WSEvents{
		cdc:           cdc,
		endpoint:      endpoint,
		remote:        remote,
		subscriptions: make(map[string]chan ctypes.ResultEvent),
		timeout:       DefaultTimeout,
	}

	w.BaseService = *libservice.NewBaseService(nil, "WSEvents", w)

	w.ws = NewWSClient(w.remote, w.endpoint, OnReconnect(func() {
		// resubscribe immediately
		w.redoSubscriptionsAfter(0 * time.Second)
	}))

	w.ws.SetCodec(w.cdc)

	return w
}

// OnStart implements libservice.Service by starting WSClient and event loop.
func (w *WSEvents) OnStart() error {
	if err := w.ws.Start(); err != nil {
		return err
	}

	go w.eventListener()

	return nil
}

func (w *WSEvents) SetLogger(logger log.Logger) {
	w.BaseService.SetLogger(logger)
	if w.ws != nil {
		w.ws.SetLogger(logger)
	}
}

func (w *WSEvents) SetTimeOut(timeout time.Duration) {
	w.timeout = timeout
}

// IsActive returns true if the client is running and not dialing.
func (c *WSEvents) IsActive() bool {
	return c.ws.IsActive()
}

// OnStop implements libservice.Service by stopping WSClient.
func (w *WSEvents) OnStop() {
	if err := w.ws.Stop(); err != nil {
		w.Logger.Error("Can't stop ws client", "err", err)
	}
}

func (w *WSEvents) PendingRequest() int {
	return len(w.ws.backlog)
}

// Subscribe implements EventsClient by using WSClient to subscribe given
// subscriber to query. By default, returns a channel with cap=1. Error is
// returned if it fails to subscribe.
//
// Channel is never closed to prevent clients from seeing an erroneous event.
//
// It returns an error if WSEvents is not running.
func (w *WSEvents) Subscribe(ctx context.Context, subscriber, query string,
	outCapacity ...int) (out <-chan ctypes.ResultEvent, err error) {
	if !w.IsRunning() {
		return nil, errNotRunning
	}

	if err := w.ws.Subscribe(ctx, query); err != nil {
		return nil, err
	}

	outCap := 1
	if len(outCapacity) > 0 {
		outCap = outCapacity[0]
	}

	outc := make(chan ctypes.ResultEvent, outCap)
	w.mtx.Lock()
	// subscriber param is ignored because Tendermint will override it with
	// remote IP anyway.
	w.subscriptions[query] = outc
	w.mtx.Unlock()

	return outc, nil
}

// Unsubscribe implements EventsClient by using WSClient to unsubscribe given
// subscriber from query.
//
// It returns an error if WSEvents is not running.
func (w *WSEvents) Unsubscribe(ctx context.Context, subscriber, query string) error {
	if !w.IsRunning() {
		return errNotRunning
	}

	if err := w.ws.Unsubscribe(ctx, query); err != nil {
		return err
	}

	w.mtx.Lock()
	_, ok := w.subscriptions[query]
	if ok {
		delete(w.subscriptions, query)
	}
	w.mtx.Unlock()

	return nil
}

// UnsubscribeAll implements EventsClient by using WSClient to unsubscribe
// given subscriber from all the queries.
//
// It returns an error if WSEvents is not running.
func (w *WSEvents) UnsubscribeAll(ctx context.Context, subscriber string) error {
	if !w.IsRunning() {
		return errNotRunning
	}

	if err := w.ws.UnsubscribeAll(ctx); err != nil {
		return err
	}

	w.mtx.Lock()
	w.subscriptions = make(map[string]chan ctypes.ResultEvent)
	w.mtx.Unlock()

	return nil
}

// After being reconnected, it is necessary to redo subscription to server
// otherwise no data will be automatically received.
func (w *WSEvents) redoSubscriptionsAfter(d time.Duration) {
	time.Sleep(d)

	w.mtx.RLock()
	defer w.mtx.RUnlock()
	for q := range w.subscriptions {
		err := w.ws.Subscribe(context.Background(), q)
		if err != nil {
			w.Logger.Error("Failed to resubscribe", "err", err)
		}
	}
}

func isErrAlreadySubscribed(err error) bool {
	return strings.Contains(err.Error(), tmpubsub.ErrAlreadySubscribed.Error())
}

func (w *WSEvents) eventListener() {
	for {
		select {
		case resp, ok := <-w.ws.ResponsesCh:
			if !ok {
				return
			}

			if resp.Error != nil {
				w.Logger.Error("WS error", "err", resp.Error.Error())
				// Error can be ErrAlreadySubscribed or max client (subscriptions per
				// client) reached or Tendermint exited.
				// We can ignore ErrAlreadySubscribed, but need to retry in other
				// cases.
				if !isErrAlreadySubscribed(resp.Error) {
					// Resubscribe after 1 second to give Tendermint time to restart (if
					// crashed).
					w.redoSubscriptionsAfter(1 * time.Second)
				}
				continue
			}

			result := new(ctypes.ResultEvent)
			err := w.cdc.UnmarshalJSON(resp.Result, result)
			if err != nil {
				w.Logger.Error("failed to unmarshal response", "err", err)
				continue
			}

			w.mtx.RLock()
			if out, ok := w.subscriptions[result.Query]; ok {
				if cap(out) == 0 {
					out <- *result
				} else {
					select {
					case out <- *result:
					default:
						w.Logger.Error("wanted to publish ResultEvent, but out channel is full", "result", result, "query", result.Query)
					}
				}
			}
			w.mtx.RUnlock()
		case <-w.Quit():
			return
		}
	}
}
func (w *WSEvents) WaitForResponse(ctx context.Context, outChan chan rpctypes.RPCResponse, result interface{}, ws *WSClient) error {
	select {
	case resp, ok := <-outChan:
		if !ok {
			return errors.New("response channel is closed")
		}
		if resp.Error != nil {
			return resp.Error
		}
		return w.cdc.UnmarshalJSON(resp.Result, result)
	case <-ctx.Done():
		err := ctx.Err()
		w.ws.reconnectAfter <- err
		return err
	}
}

func (w *WSEvents) SimpleCall(doRpc func(ctx context.Context) error, ws *WSClient, proto interface{}) error {
	outChan := make(chan rpctypes.RPCResponse, 1)
	defer close(outChan)

	ctx, cancel := context.WithTimeout(context.Background(), w.timeout)
	defer cancel()

	err := doRpc(ctx)

	// req id in incremented in WSClient.Call()
	id := w.ws.nextReqID
	w.responseChanMap.Store(id, outChan)
	defer w.responseChanMap.Delete(id)

	if err != nil {
		return err
	}

	return w.WaitForResponse(ctx, outChan, proto, ws)
}

func (w *WSEvents) Status() (*ctypes.ResultStatus, error) {
	status := new(ctypes.ResultStatus)
	err := w.SimpleCall(w.ws.Status, w.ws, status)
	return status, err
}

func (w *WSEvents) ABCIInfo() (*ctypes.ResultABCIInfo, error) {
	info := new(ctypes.ResultABCIInfo)
	err := w.SimpleCall(w.ws.ABCIInfo, w.ws, info)
	return info, err
}

func (w *WSEvents) ABCIQueryWithOptions(path string, data libbytes.HexBytes, opts client.ABCIQueryOptions) (*ctypes.ResultABCIQuery, error) {
	abciQuery := new(ctypes.ResultABCIQuery)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.ABCIQueryWithOptions(ctx, path, data, opts)
	}, w.ws, abciQuery)
	return abciQuery, err
}

func (w *WSEvents) BroadcastTxCommit(tx types.Tx) (*ResultBroadcastTxCommit, error) {
	txCommit := new(ResultBroadcastTxCommit)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.BroadcastTxCommit(ctx, tx)
	}, w.ws, txCommit)
	if err == nil {
		txCommit.complement()
	}
	return txCommit, err
}

func (w *WSEvents) BroadcastTx(route string, tx types.Tx) (*ctypes.ResultBroadcastTx, error) {
	txRes := new(ctypes.ResultBroadcastTx)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.BroadcastTx(ctx, route, tx)
	}, w.ws, txRes)
	return txRes, err
}

func (w *WSEvents) UnconfirmedTxs(limit int) (*ctypes.ResultUnconfirmedTxs, error) {
	unConfirmTxs := new(ctypes.ResultUnconfirmedTxs)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.UnconfirmedTxs(ctx, limit)
	}, w.ws, unConfirmTxs)
	return unConfirmTxs, err
}

func (w *WSEvents) NumUnconfirmedTxs() (*ctypes.ResultUnconfirmedTxs, error) {
	numUnConfirmTxs := new(ctypes.ResultUnconfirmedTxs)
	err := w.SimpleCall(w.ws.NumUnconfirmedTxs, w.ws, numUnConfirmTxs)
	return numUnConfirmTxs, err
}

func (w *WSEvents) NetInfo() (*ctypes.ResultNetInfo, error) {
	netInfo := new(ctypes.ResultNetInfo)
	err := w.SimpleCall(w.ws.NetInfo, w.ws, netInfo)
	return netInfo, err
}

func (w *WSEvents) DumpConsensusState() (*ctypes.ResultDumpConsensusState, error) {
	consensusState := new(ctypes.ResultDumpConsensusState)
	err := w.SimpleCall(w.ws.DumpConsensusState, w.ws, consensusState)
	return consensusState, err
}

func (w *WSEvents) ConsensusState() (*ctypes.ResultConsensusState, error) {
	consensusState := new(ctypes.ResultConsensusState)
	err := w.SimpleCall(w.ws.ConsensusState, w.ws, consensusState)
	return consensusState, err
}

func (w *WSEvents) Health() (*ctypes.ResultHealth, error) {
	health := new(ctypes.ResultHealth)
	err := w.SimpleCall(w.ws.Health, w.ws, health)
	return health, err
}

func (w *WSEvents) BlockchainInfo(minHeight, maxHeight int64) (*ctypes.ResultBlockchainInfo, error) {
	blocksInfo := new(ctypes.ResultBlockchainInfo)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.BlockchainInfo(ctx, minHeight, maxHeight)
	}, w.ws, blocksInfo)
	return blocksInfo, err
}

func (w *WSEvents) Genesis() (*ctypes.ResultGenesis, error) {
	genesis := new(ctypes.ResultGenesis)
	err := w.SimpleCall(w.ws.Genesis, w.ws, genesis)
	return genesis, err
}

func (w *WSEvents) Block(height *int64) (*ctypes.ResultBlock, error) {
	block := new(ctypes.ResultBlock)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.Block(ctx, height)
	}, w.ws, block)
	return block, err
}

func (w *WSEvents) BlockResults(height *int64) (*ResultBlockResults, error) {
	block := new(ResultBlockResults)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.BlockResults(ctx, height)
	}, w.ws, block)
	if err == nil {
		block.complement()
	}
	return block, err
}

func (w *WSEvents) Commit(height *int64) (*ctypes.ResultCommit, error) {
	commit := new(ctypes.ResultCommit)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.Commit(ctx, height)
	}, w.ws, commit)
	return commit, err
}

func (w *WSEvents) Tx(hash []byte, prove bool) (*ResultTx, error) {
	tx := new(ResultTx)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.Tx(ctx, hash, prove)
	}, w.ws, tx)
	if err == nil {
		tx.complement()
	}
	return tx, err
}

func (w *WSEvents) TxSearch(query string, prove bool, page, perPage int) (*ResultTxSearch, error) {
	txs := new(ResultTxSearch)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.TxSearch(ctx, query, prove, page, perPage)
	}, w.ws, txs)
	if err == nil {
		txs.complement()
	}
	return txs, err
}

func (w *WSEvents) TxInfoSearch(query string, prove bool, page, perPage int) ([]Info, error) {
	txs := new(ResultTxSearch)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.TxSearch(ctx, query, prove, page, perPage)
	}, w.ws, txs)
	if err != nil {
		return nil, err
	}
	return FormatTxResults(w.cdc, txs.Txs)
}

func (w *WSEvents) Validators(height *int64) (*ctypes.ResultValidators, error) {
	validators := new(ctypes.ResultValidators)
	err := w.SimpleCall(func(ctx context.Context) error {
		return w.ws.Validators(ctx, height)
	}, w.ws, validators)
	return validators, err
}

// WSClient is a WebSocket client. The methods of WSClient are safe for use by
// multiple goroutines.
type WSClient struct {
	conn *websocket.Conn
	cdc  *amino.Codec

	Address  string // IP:PORT or /path/to/socket
	Endpoint string // /websocket/url/endpoint
	Dialer   func(string, string) (net.Conn, error)

	// Single user facing channel to read RPCResponses from
	ResponsesCh chan rpctypes.RPCResponse

	// Callback, which will be called each time after successful reconnect.
	onReconnect func()

	// internal channels
	send            chan rpctypes.RPCRequest // user requests
	backlog         chan rpctypes.RPCRequest // stores a single user request received during a conn failure
	reconnectAfter  chan error               // reconnect requests
	readRoutineQuit chan struct{}            // a way for readRoutine to close writeRoutine

	// Maximum reconnect attempts (0 or greater; default: 25).
	maxReconnectAttempts int

	// Support both ws and wss protocols
	protocol string

	wg sync.WaitGroup

	mtx            sync.RWMutex
	dialing        atomic.Value
	sentLastPingAt time.Time
	reconnecting   bool
	nextReqID      int

	// Time allowed to write a message to the server. 0 means block until operation succeeds.
	writeWait time.Duration

	// Time allowed to read the next message from the server. 0 means block until operation succeeds.
	readWait time.Duration

	// Send pings to server with this period. Must be less than readWait. If 0, no pings will be sent.
	pingPeriod time.Duration

	libservice.BaseService
}

// NewWSClient returns a new client. See the commentary on the func(*WSClient)
// functions for a detailed description of how to configure ping period and
// pong wait time. The endpoint argument must begin with a `/`.
func NewWSClient(remoteAddr, endpoint string, options ...func(*WSClient)) *WSClient {
	protocol, addr, dialer := makeHTTPDialer(remoteAddr)

	// default to ws protocol, unless wss is explicitly specified
	if protocol != "wss" {
		protocol = "ws"
	}

	c := &WSClient{
		Address:  addr,
		Dialer:   dialer,
		Endpoint: endpoint,

		cdc:                  amino.NewCodec(),
		maxReconnectAttempts: defaultMaxReconnectAttempts,
		readWait:             defaultReadWait,
		writeWait:            defaultWriteWait,
		pingPeriod:           defaultPingPeriod,
		protocol:             protocol,
		send:                 make(chan rpctypes.RPCRequest),
	}

	c.dialing.Store(true)
	c.BaseService = *libservice.NewBaseService(nil, "WSClient", c)
	for _, option := range options {
		option(c)
	}

	return c
}

// MaxReconnectAttempts sets the maximum number of reconnect attempts before returning an error.
// It should only be used in the constructor and is not Goroutine-safe.
func MaxReconnectAttempts(max int) func(*WSClient) {
	return func(c *WSClient) {
		c.maxReconnectAttempts = max
	}
}

// ReadWait sets the amount of time to wait before a websocket read times out.
// It should only be used in the constructor and is not Goroutine-safe.
func ReadWait(readWait time.Duration) func(*WSClient) {
	return func(c *WSClient) {
		c.readWait = readWait
	}
}

// WriteWait sets the amount of time to wait before a websocket write times out.
// It should only be used in the constructor and is not Goroutine-safe.
func WriteWait(writeWait time.Duration) func(*WSClient) {
	return func(c *WSClient) {
		c.writeWait = writeWait
	}
}

// PingPeriod sets the duration for sending websocket pings.
// It should only be used in the constructor - not Goroutine-safe.
func PingPeriod(pingPeriod time.Duration) func(*WSClient) {
	return func(c *WSClient) {
		c.pingPeriod = pingPeriod
	}
}

// OnReconnect sets the callback, which will be called every time after
// successful reconnect.
func OnReconnect(cb func()) func(*WSClient) {
	return func(c *WSClient) {
		c.onReconnect = cb
	}
}

// String returns WS client full address.
func (c *WSClient) String() string {
	if c.conn != nil {
		return fmt.Sprintf("%s, local: %s, remote: %s", c.Address, c.conn.LocalAddr().String(), c.conn.RemoteAddr().String())
	}
	return fmt.Sprintf("%s (%s)", c.Address, c.Endpoint)
}

// OnStart implements libservice.Service by dialing a server and creating read and
// write routines.
func (c *WSClient) OnStart() error {
	err := c.dial()
	if err != nil {
		return err
	}

	c.ResponsesCh = make(chan rpctypes.RPCResponse)

	c.send = make(chan rpctypes.RPCRequest)
	// 1 additional error may come from the read/write
	// goroutine depending on which failed first.
	c.reconnectAfter = make(chan error, 1)
	// capacity for 1 request. a user won't be able to send more because the send
	// channel is unbuffered.
	c.backlog = make(chan rpctypes.RPCRequest, 1)

	c.startReadWriteRoutines()
	go c.reconnectRoutine()

	return nil
}

// Stop overrides service.Service#Stop. There is no other way to wait until Quit
// channel is closed.
func (c *WSClient) Stop() error {
	if err := c.BaseService.Stop(); err != nil {
		return err
	}
	// only close user-facing channels when we can't write to them
	c.wg.Wait()
	close(c.ResponsesCh)

	return nil
}

// IsReconnecting returns true if the client is reconnecting right now.
func (c *WSClient) IsReconnecting() bool {
	c.mtx.RLock()
	defer c.mtx.RUnlock()
	return c.reconnecting
}

// IsActive returns true if the client is running and not dialing.
func (c *WSClient) IsActive() bool {
	return c.IsRunning() && !c.IsReconnecting()
}

// Send the given RPC request to the server. Results will be available on
// responsesCh, errors, if any, on ErrorsCh. Will block until send succeeds or
// ctx.Done is closed.
func (c *WSClient) Send(ctx context.Context, request rpctypes.RPCRequest) error {
	select {
	case c.send <- request:
		c.Logger.Debug("sent a request", "req", request)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Call the given method. See Send description.
func (c *WSClient) Call(ctx context.Context, method string, params map[string]interface{}) error {
	request, err := rpctypes.MapToRequest(c.nextRequestID(), method, params)
	if err != nil {
		return err
	}
	return c.Send(ctx, request)
}

func (c *WSClient) Codec() *amino.Codec {
	return c.cdc
}

func (c *WSClient) SetCodec(cdc *amino.Codec) {
	c.cdc = cdc
}

func (c *WSClient) GenRequestId() (rpctypes.JSONRPCStringID, error) {
	id, err := uuid.NewV4()
	if err != nil {
		return "", err
	}
	return rpctypes.JSONRPCStringID(id.String()), nil
}

///////////////////////////////////////////////////////////////////////////////
// Private methods

func (c *WSClient) nextRequestID() rpctypes.JSONRPCIntID {
	c.mtx.Lock()
	id := c.nextReqID
	c.nextReqID++
	c.mtx.Unlock()
	return rpctypes.JSONRPCIntID(id)
}

func (c *WSClient) dial() error {
	dialer := &websocket.Dialer{
		NetDial: c.Dialer,
		Proxy:   http.ProxyFromEnvironment,
	}
	rHeader := http.Header{}
	conn, _, err := dialer.Dial(c.protocol+"://"+c.Address+c.Endpoint, rHeader)
	if err != nil {
		return err
	}
	c.conn = conn
	return nil
}

// reconnect tries to redial up to maxReconnectAttempts with exponential
// backoff.
func (c *WSClient) reconnect() error {
	attempt := 0

	c.mtx.Lock()
	c.reconnecting = true
	c.mtx.Unlock()
	defer func() {
		c.mtx.Lock()
		c.reconnecting = false
		c.mtx.Unlock()
	}()

	for {
		jitter := time.Duration(tmrand.Float64() * float64(time.Second)) // 1s == (1e9 ns)
		backoffDuration := jitter + ((1 << uint(attempt)) * time.Second)

		c.Logger.Info("reconnecting", "attempt", attempt+1, "backoff_duration", backoffDuration)
		time.Sleep(backoffDuration)

		err := c.dial()
		if err != nil {
			c.Logger.Error("failed to redial", "err", err)
		} else {
			c.Logger.Info("reconnected")
			if c.onReconnect != nil {
				go c.onReconnect()
			}
			return nil
		}

		attempt++

		if attempt > c.maxReconnectAttempts {
			return fmt.Errorf("reached maximum reconnect attempts: %w", err)
		}
	}
}

func (c *WSClient) startReadWriteRoutines() {
	c.wg.Add(2)
	c.readRoutineQuit = make(chan struct{})
	go c.readRoutine()
	go c.writeRoutine()
}

func (c *WSClient) processBacklog() error {
	select {
	case request := <-c.backlog:
		if c.writeWait > 0 {
			if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeWait)); err != nil {
				c.Logger.Error("failed to set write deadline", "err", err)
			}
		}
		if err := c.conn.WriteJSON(request); err != nil {
			c.Logger.Error("failed to resend request", "err", err)
			c.reconnectAfter <- err
			// requeue request
			c.backlog <- request
			return err
		}
		c.Logger.Info("resend a request", "req", request)
	default:
	}
	return nil
}

func (c *WSClient) reconnectRoutine() {
	for {
		select {
		case originalError := <-c.reconnectAfter:
			// wait until writeRoutine and readRoutine finish
			c.wg.Wait()
			if err := c.reconnect(); err != nil {
				c.Logger.Error("failed to reconnect", "err", err, "original_err", originalError)
				if err = c.Stop(); err != nil {
					c.Logger.Error("failed to stop conn", "error", err)
				}

				return
			}
			// drain reconnectAfter
		LOOP:
			for {
				select {
				case <-c.reconnectAfter:
				default:
					break LOOP
				}
			}
			err := c.processBacklog()
			if err == nil {
				c.startReadWriteRoutines()
			}

		case <-c.Quit():
			return
		}
	}
}

// The client ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *WSClient) writeRoutine() {
	var ticker *time.Ticker
	if c.pingPeriod > 0 {
		// ticker with a predefined period
		ticker = time.NewTicker(c.pingPeriod)
	} else {
		// ticker that never fires
		ticker = &time.Ticker{C: make(<-chan time.Time)}
	}

	defer func() {
		ticker.Stop()
		c.conn.Close()
		// err != nil {
		// ignore error; it will trigger in tests
		// likely because it's closing an already closed connection
		// }
		c.wg.Done()
	}()

	for {
		select {
		case request := <-c.send:
			if c.writeWait > 0 {
				if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeWait)); err != nil {
					c.Logger.Error("failed to set write deadline", "err", err)
				}
			}
			if err := c.conn.WriteJSON(request); err != nil {
				c.Logger.Error("failed to send request", "err", err)
				c.reconnectAfter <- err
				// add request to the backlog, so we don't lose it
				c.backlog <- request
				return
			}
		case <-ticker.C:
			if c.writeWait > 0 {
				if err := c.conn.SetWriteDeadline(time.Now().Add(c.writeWait)); err != nil {
					c.Logger.Error("failed to set write deadline", "err", err)
				}
			}
			if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				c.Logger.Error("failed to write ping", "err", err)
				c.reconnectAfter <- err
				return
			}
			c.mtx.Lock()
			c.sentLastPingAt = time.Now()
			c.mtx.Unlock()
			c.Logger.Debug("sent ping")
		case <-c.readRoutineQuit:
			return
		case <-c.Quit():
			if err := c.conn.WriteMessage(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			); err != nil {
				c.Logger.Error("failed to write message", "err", err)
			}
			return
		}
	}
}

// The client ensures that there is at most one reader to a connection by
// executing all reads from this goroutine.
func (c *WSClient) readRoutine() {
	defer func() {
		c.conn.Close()
		c.wg.Done()
	}()

	for {
		// reset deadline for every message type (control or data)
		if c.readWait > 0 {
			if err := c.conn.SetReadDeadline(time.Now().Add(c.readWait)); err != nil {
				c.Logger.Error("failed to set read deadline", "err", err)
			}
		}
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if !websocket.IsUnexpectedCloseError(err, websocket.CloseNormalClosure) {
				return
			}

			c.Logger.Error("failed to read response", "err", err)
			close(c.readRoutineQuit)
			c.reconnectAfter <- err
			return
		}

		var response rpctypes.RPCResponse
		err = json.Unmarshal(data, &response)
		if err != nil {
			c.Logger.Error("failed to parse response", "err", err, "data", string(data))
			continue
		}

		c.Logger.Info("got response", "id", response.ID, "result", fmt.Sprintf("%X", response.Result))

		select {
		case <-c.Quit():
		case c.ResponsesCh <- response:
		}
	}
}

///////////////////////////////////////////////////////////////////////////////
// Predefined methods

// Subscribe to a query. Note the server must have a "subscribe" route
// defined.
func (c *WSClient) Subscribe(ctx context.Context, query string) error {
	params := map[string]interface{}{"query": query}
	return c.Call(ctx, "subscribe", params)
}

// Unsubscribe from a query. Note the server must have a "unsubscribe" route
// defined.
func (c *WSClient) Unsubscribe(ctx context.Context, query string) error {
	params := map[string]interface{}{"query": query}

	return c.Call(ctx, "unsubscribe", params)
}

// UnsubscribeAll from all. Note the server must have a "unsubscribe_all" route
// defined.
func (c *WSClient) UnsubscribeAll(ctx context.Context) error {
	params := map[string]interface{}{}
	return c.Call(ctx, "unsubscribe_all", params)
}

// JSONRPC Methods
func (c *WSClient) Status(ctx context.Context) error {
	return c.Call(ctx, "status", map[string]interface{}{})
}

func (c *WSClient) ABCIInfo(ctx context.Context) error {
	return c.Call(ctx, "abci_info", map[string]interface{}{})
}

func (c *WSClient) ABCIQueryWithOptions(ctx context.Context, path string, data libbytes.HexBytes, opts client.ABCIQueryOptions) error {
	return c.Call(ctx, "abci_query", map[string]interface{}{"path": path, "data": data, "height": opts.Height, "prove": opts.Prove})
}

func (c *WSClient) BroadcastTxCommit(ctx context.Context, tx types.Tx) error {
	return c.Call(ctx, "broadcast_tx_commit", map[string]interface{}{"tx": tx})
}

func (c *WSClient) BroadcastTx(ctx context.Context, route string, tx types.Tx) error {
	return c.Call(ctx, route, map[string]interface{}{"tx": tx})
}

func (c *WSClient) UnconfirmedTxs(ctx context.Context, limit int) error {
	return c.Call(ctx, "unconfirmed_txs", map[string]interface{}{"limit": limit})
}

func (c *WSClient) NumUnconfirmedTxs(ctx context.Context) error {
	return c.Call(ctx, "num_unconfirmed_txs", map[string]interface{}{})
}

func (c *WSClient) NetInfo(ctx context.Context) error {
	return c.Call(ctx, "net_info", map[string]interface{}{})
}

func (c *WSClient) DumpConsensusState(ctx context.Context) error {
	return c.Call(ctx, "dump_consensus_state", map[string]interface{}{})
}

func (c *WSClient) ConsensusState(ctx context.Context) error {
	return c.Call(ctx, "consensus_state", map[string]interface{}{})
}

func (c *WSClient) Health(ctx context.Context) error {
	return c.Call(ctx, "health", map[string]interface{}{})
}

func (c *WSClient) BlockchainInfo(ctx context.Context, minHeight, maxHeight int64) error {
	return c.Call(ctx, "blockchain",
		map[string]interface{}{"minHeight": minHeight, "maxHeight": maxHeight})
}

func (c *WSClient) Genesis(ctx context.Context) error {
	return c.Call(ctx, "genesis", map[string]interface{}{})
}

func (c *WSClient) Block(ctx context.Context, height *int64) error {
	return c.Call(ctx, "block", map[string]interface{}{"height": height})
}

func (c *WSClient) BlockResults(ctx context.Context, height *int64) error {
	return c.Call(ctx, "block_results", map[string]interface{}{"height": height})
}

func (c *WSClient) Commit(ctx context.Context, height *int64) error {
	return c.Call(ctx, "commit", map[string]interface{}{"height": height})
}
func (c *WSClient) Tx(ctx context.Context, hash []byte, prove bool) error {
	params := map[string]interface{}{
		"hash":  hash,
		"prove": prove,
	}
	return c.Call(ctx, "tx", params)
}

func (c *WSClient) TxSearch(ctx context.Context, query string, prove bool, page, perPage int) error {
	params := map[string]interface{}{
		"query":    query,
		"prove":    prove,
		"page":     page,
		"per_page": perPage,
	}
	return c.Call(ctx, "tx_search", params)
}

func (c *WSClient) Validators(ctx context.Context, height *int64) error {
	return c.Call(ctx, "validators", map[string]interface{}{"height": height})
}

func makeHTTPDialer(remoteAddr string) (string, string, func(string, string) (net.Conn, error)) {
	// protocol to use for http operations, to support both http and https
	clientProtocol := protoHTTP

	parts := strings.SplitN(remoteAddr, "://", 2)
	var protocol, address string
	if len(parts) == 1 {
		// default to tcp if nothing specified
		protocol, address = protoTCP, remoteAddr
	} else if len(parts) == 2 {
		protocol, address = parts[0], parts[1]
	} else {
		// return a invalid message
		msg := fmt.Sprintf("Invalid addr: %s", remoteAddr)
		return clientProtocol, msg, func(_ string, _ string) (net.Conn, error) {
			return nil, errors.New(msg)
		}
	}

	// accept http as an alias for tcp and set the client protocol
	switch protocol {
	case protoHTTP, protoHTTPS:
		clientProtocol = protocol
		protocol = protoTCP
	case protoWS, protoWSS:
		clientProtocol = protocol
	}

	// replace / with . for http requests (kvstore domain)
	trimmedAddress := strings.Replace(address, "/", ".", -1)
	return clientProtocol, trimmedAddress, func(proto, addr string) (net.Conn, error) {
		return net.DialTimeout(protocol, address, DefaultTimeout)
	}
}

// parse the indexed txs into an array of Info
func FormatTxResults(cdc *amino.Codec, res []*ResultTx) ([]Info, error) {
	var err error
	out := make([]Info, len(res))
	for i := range res {
		out[i], err = formatTxResult(cdc, res[i])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func formatTxResult(cdc *amino.Codec, res *ResultTx) (Info, error) {
	parsedTx, err := ParseTx(cdc, res.Tx)
	if err != nil {
		return Info{}, err
	}
	return Info{
		Hash:   res.Hash,
		Height: res.Height,
		Tx:     parsedTx,
		Result: res.TxResult,
	}, nil
}

func ParseTx(cdc *amino.Codec, txBytes []byte) (tx.Tx, error) {
	var parsedTx tx.StdTx
	err := cdc.UnmarshalBinaryLengthPrefixed(txBytes, &parsedTx)

	if err != nil {
		return nil, err
	}

	return parsedTx, nil
}
