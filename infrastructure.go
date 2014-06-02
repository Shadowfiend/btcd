// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package btcrpcclient

import (
	"bytes"
	"container/list"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/conformal/btcjson"
	"github.com/conformal/btcws"
	"github.com/conformal/go-socks"
	"github.com/gorilla/websocket"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrInvalidAuth is an error to describe the condition where the client
	// is either unable to authenticate or the specified endpoint is
	// incorrect.
	ErrInvalidAuth = errors.New("authentication failure")

	// ErrInvalidEndpoint is an error to describe the condition where the
	// websocket handshake failed with the specified endpoint.
	ErrInvalidEndpoint = errors.New("the endpoint either does not support " +
		"websockets or does not exist")

	// ErrClientDisconnect is an error to describe the condition where the
	// client has been disconnected from the RPC server.  When the
	// DisableAutoReconnect option is not set, any outstanding futures
	// when a client disconnect occurs will return this error as will
	// any new requests.
	ErrClientDisconnect = errors.New("the client has been disconnected")

	// ErrClientShutdown is an error to describe the condition where the
	// client is either already shutdown, or in the process of shutting
	// down.  Any outstanding futures when a client shutdown occurs will
	// return this error as will any new requests.
	ErrClientShutdown = errors.New("the client has been shutdown")
)

const (
	// sendBufferSize is the number of elements the websocket send channel
	// can queue before blocking.
	sendBufferSize = 50

	// sendPostBufferSize is the number of elements the HTTP POST send
	// channel can queue before blocking.
	sendPostBufferSize = 100

	// connectionRetryInterval is the amount of time to wait in between
	// retries when automatically reconnecting to an RPC server.
	connectionRetryInterval = time.Second * 5
)

// futureResult holds information about a future promise to deliver the result
// of an asynchronous request.
type futureResult struct {
	reply *btcjson.Reply
	err   error
}

// sendPostDetails houses an HTTP POST request to send to an RPC server as well
// as the original JSON-RPC command and a channel to reply on when the server
// responds with the result.
type sendPostDetails struct {
	command      btcjson.Cmd
	request      *http.Request
	responseChan chan *futureResult
}

// jsonRequest holds information about a json request that is used to properly
// detect, interpret, and deliver a reply to it.
type jsonRequest struct {
	cmd          btcjson.Cmd
	responseChan chan *futureResult
}

// Client represents a Bitcoin RPC client which allows easy access to the
// various RPC methods available on a Bitcoin RPC server.  Each of the wrapper
// functions handle the details of converting the passed and return types to and
// from the underlying JSON types which are required for the JSON-RPC
// invocations
//
// The client provides each RPC in both synchronous (blocking) and asynchronous
// (non-blocking) forms.  The asynchronous forms are based on the concept of
// futures where they return an instance of a type that promises to deliver the
// result of the invocation at some future time.  Invoking the Receive method on
// the returned future will block until the result is available if it's not
// already.
type Client struct {
	id uint64 // atomic, so must stay 64-bit aligned

	// config holds the connection configuration assoiated with this client.
	config *ConnConfig

	// wsConn is the underlying websocket connection when not in HTTP POST
	// mode.
	wsConn *websocket.Conn

	// httpClient is the underlying HTTP client to use when running in HTTP
	// POST mode.
	httpClient *http.Client

	// mtx is a mutex to protect access to connection related fields.
	mtx sync.Mutex

	// disconnected indicated whether or not the server is disconnected.
	disconnected bool

	// retryCount holds the number of times the client has tried to
	// reconnect to the RPC server.
	retryCount int64

	// Track command and their response channels by ID.
	requestLock sync.Mutex
	requestMap  map[uint64]*list.Element
	requestList *list.List

	// Notifications.
	ntfnHandlers *NotificationHandlers
	ntfnState    *notificationState

	// Networking infrastructure.
	sendChan     chan []byte
	sendPostChan chan *sendPostDetails
	disconnect   chan struct{}
	shutdown     chan struct{}
	wg           sync.WaitGroup
}

// NextID returns the next id to be used when sending a JSON-RPC message.  This
// ID allows responses to be associated with particular requests per the
// JSON-RPC specification.  Typically the consumer of the client does not need
// to call this function, however, if a custom request is being created and used
// this function should be used to ensure the ID is unique amongst all requests
// being made.
func (c *Client) NextID() uint64 {
	return atomic.AddUint64(&c.id, 1)
}

// addRequest associates the passed jsonRequest with the passed id.  This allows
// the response from the remote server to be unmarshalled to the appropriate
// type and sent to the specified channel when it is received.
//
// This function is safe for concurrent access.
func (c *Client) addRequest(id uint64, request *jsonRequest) {
	c.requestLock.Lock()
	defer c.requestLock.Unlock()

	// TODO(davec): Already there?
	element := c.requestList.PushBack(request)
	c.requestMap[id] = element
}

// removeRequest returns and removes the jsonRequest which contains the response
// channel and original method associated with the passed id or nil if there is
// no association.
//
// This function is safe for concurrent access.
func (c *Client) removeRequest(id uint64) *jsonRequest {
	c.requestLock.Lock()
	defer c.requestLock.Unlock()

	element := c.requestMap[id]
	if element != nil {
		delete(c.requestMap, id)
		request := c.requestList.Remove(element).(*jsonRequest)
		return request
	}

	return nil
}

// removeAllRequests removes all the jsonRequests which contain the response
// channels for outstanding requests.
//
// This function is safe for concurrent access.
func (c *Client) removeAllRequests() {
	c.requestLock.Lock()
	defer c.requestLock.Unlock()

	c.requestMap = make(map[uint64]*list.Element)
	c.requestList.Init()
}

// trackRegisteredNtfns examines the passed command to see if it is one of
// the notification commands and updates the notification state that is used
// to automatically re-establish registered notifications on reconnects.
func (c *Client) trackRegisteredNtfns(cmd btcjson.Cmd) {
	// Nothing to do if the caller is not interested in notifications.
	if c.ntfnHandlers == nil {
		return
	}

	c.ntfnState.Lock()
	defer c.ntfnState.Unlock()

	switch bcmd := cmd.(type) {
	case *btcws.NotifyBlocksCmd:
		c.ntfnState.notifyBlocks = true

	case *btcws.NotifyNewTransactionsCmd:
		if bcmd.Verbose {
			c.ntfnState.notifyNewTxVerbose = true
		} else {
			c.ntfnState.notifyNewTx = true

		}

	case *btcws.NotifySpentCmd:
		for _, op := range bcmd.OutPoints {
			c.ntfnState.notifySpent[op] = struct{}{}
		}

	case *btcws.NotifyReceivedCmd:
		for _, addr := range bcmd.Addresses {
			c.ntfnState.notifyReceived[addr] = struct{}{}
		}
	}
}

// handleMessage is the main handler for incoming requests.  It enforces
// authentication, parses the incoming json, looks up and executes handlers
// (including pass through for standard RPC commands), sends the appropriate
// response.  It also detects commands which are marked as long-running and
// sends them off to the asyncHander for processing.
func (c *Client) handleMessage(msg []byte) {
	// Attempt to unmarshal the message as a known JSON-RPC command.
	if cmd, err := btcjson.ParseMarshaledCmd(msg); err == nil {
		// Commands that have an ID associated with them are not
		// notifications.  Since this is a client, it should not
		// be receiving non-notifications.
		if cmd.Id() != nil {
			// Invalid response
			log.Warnf("Remote server sent a non-notification "+
				"JSON-RPC Request (Id: %v)", cmd.Id())
			return
		}

		// Deliver the notification.
		log.Tracef("Received notification [%s]", cmd.Method())
		c.handleNotification(cmd)
		return
	}

	// The message was not a command/notification, so it should be a reply
	// to a previous request.

	var r btcjson.Reply
	if err := json.Unmarshal([]byte(msg), &r); err != nil {
		log.Warnf("Unable to unmarshal inbound message as " +
			"notification or response")
		return
	}

	// Ensure the reply has an id.
	if r.Id == nil {
		log.Warnf("Received response with no id")
		return
	}

	// Ensure the id is the expected type.
	fid, ok := (*r.Id).(float64)
	if !ok {
		log.Warnf("Received unexpected id type: %T (value %v)",
			*r.Id, *r.Id)
		return
	}
	id := uint64(fid)
	log.Tracef("Received response for id %d (result %v)", id, r.Result)
	request := c.removeRequest(id)

	// Nothing more to do if there is no request associated with this reply.
	if request == nil || request.responseChan == nil {
		log.Warnf("Received unexpected reply: %s (id %d)", r.Result, id)
		return
	}

	// Unmarshal the reply into a concrete result if possible and deliver
	// it to the associated channel.
	reply, err := btcjson.ReadResultCmd(request.cmd.Method(), []byte(msg))
	if err != nil {
		log.Warnf("Failed to unmarshal reply to command [%s] "+
			"(id %d): %v", request.cmd.Method(), id, err)
		request.responseChan <- &futureResult{reply: nil, err: err}
		return
	}

	// Since the command was successful, examine it to see if it's a
	// notification, and if is, add it to the notification state so it
	// can automatically be re-established on reconnect.
	c.trackRegisteredNtfns(request.cmd)

	// Deliver the reply.
	request.responseChan <- &futureResult{reply: &reply, err: nil}
}

// wsInHandler handles all incoming messages for the websocket connection
// associated with the client.  It must be run as a goroutine.
func (c *Client) wsInHandler() {
out:
	for {
		// Break out of the loop once the shutdown channel has been
		// closed.  Use a non-blocking select here so we fall through
		// otherwise.
		select {
		case <-c.shutdown:
			break out
		default:
		}

		_, msg, err := c.wsConn.ReadMessage()
		if err != nil {
			// Log the error if it's not due to disconnecting.
			if _, ok := err.(*net.OpError); !ok {
				log.Errorf("Websocket receive error from "+
					"%s: %v", c.config.Host, err)
			}
			break out
		}
		c.handleMessage(msg)
	}

	// Ensure the connection is closed.
	c.Disconnect()
	c.wg.Done()
	log.Tracef("RPC client input handler done for %s", c.config.Host)
}

// wsOutHandler handles all outgoing messages for the websocket connection.  It
// uses a buffered channel to serialize output messages while allowing the
// sender to continue running asynchronously.  It must be run as a goroutine.
func (c *Client) wsOutHandler() {
out:
	for {
		// Send any messages ready for send until the client is
		// disconnected closed.
		select {
		case msg := <-c.sendChan:
			err := c.wsConn.WriteMessage(websocket.TextMessage, msg)
			if err != nil {
				c.Disconnect()
				break out
			}

		case <-c.disconnect:
			break out
		}
	}

	// Drain any channels before exiting so nothing is left waiting around
	// to send.
cleanup:
	for {
		select {
		case <-c.sendChan:
		default:
			break cleanup
		}
	}
	c.wg.Done()
	log.Tracef("RPC client output handler done for %s", c.config.Host)
}

// sendMessage sends the passed JSON to the connected server using the
// websocket connection.  It is backed by a buffered channel, so it will not
// block until the send channel is full.
func (c *Client) sendMessage(marshalledJSON []byte) {
	// Don't send the message if disconnected.
	if c.Disconnected() {
		return
	}

	c.sendChan <- marshalledJSON
}

// reregisterNtfns creates and sends commands needed to re-establish the current
// notification state associated with the client.  It should only be called on
// on reconnect by the resendCmds function.
func (c *Client) reregisterNtfns() error {
	// Nothing to do if the caller is not interested in notifications.
	if c.ntfnHandlers == nil {
		return nil
	}

	// In order to avoid holding the lock on the notification state for the
	// entire time of the potentially long running RPCs issued below, make a
	// copy of it and work from that.
	//
	// Also, other commands will be running concurrently which could modify
	// the notification state (while not under the lock of course) which
	// also register it with the remote RPC server, so this prevents double
	// registrations.
	stateCopy := c.ntfnState.Copy()

	// Reregister notifyblocks if needed.
	if stateCopy.notifyBlocks {
		log.Debugf("Reregistering [notifyblocks]")
		if err := c.NotifyBlocks(); err != nil {
			return err
		}
	}

	// Reregister notifynewtransactions if needed.
	if stateCopy.notifyNewTx || stateCopy.notifyNewTxVerbose {
		log.Debugf("Reregistering [notifynewtransactions] (verbose=%v)",
			stateCopy.notifyNewTxVerbose)
		err := c.NotifyNewTransactions(stateCopy.notifyNewTxVerbose)
		if err != nil {
			return err
		}
	}

	// Reregister the combination of all previously registered notifyspent
	// outpoints in one command if needed.
	nslen := len(stateCopy.notifySpent)
	if nslen > 0 {
		outpoints := make([]btcws.OutPoint, 0, nslen)
		for op := range stateCopy.notifySpent {
			outpoints = append(outpoints, op)
		}
		log.Debugf("Reregistering [notifyspent] outpoints: %v", outpoints)
		if err := c.notifySpentInternal(outpoints).Receive(); err != nil {
			return err
		}
	}

	// Reregister the combination of all previously registered
	// notifyreceived addresses in one command if needed.
	nrlen := len(stateCopy.notifyReceived)
	if nrlen > 0 {
		addresses := make([]string, 0, nrlen)
		for addr := range stateCopy.notifyReceived {
			addresses = append(addresses, addr)
		}
		log.Debugf("Reregistering [notifyreceived] addresses: %v", addresses)
		if err := c.notifyReceivedInternal(addresses).Receive(); err != nil {
			return err
		}
	}

	return nil
}

// resendCmds resends any commands that had not completed when the client
// disconnected.  It is intended to be called once the client has reconnected as
// a separate goroutine.
func (c *Client) resendCmds() {
	// Set the notification state back up.  If anything goes wrong,
	// disconnect the client.
	if err := c.reregisterNtfns(); err != nil {
		log.Warnf("Unable to re-establish notification state: %v", err)
		c.Disconnect()
		return
	}

	// Since it's possible to block on send and more commands might be
	// added by the caller while resending, make a copy of all of the
	// commands that need to be resent now and work from the copy.  This
	// also allows the lock to be released quickly.
	c.requestLock.Lock()
	resendCmds := make([]*jsonRequest, 0, c.requestList.Len())
	for e := c.requestList.Front(); e != nil; e = e.Next() {
		req := e.Value.(*jsonRequest)
		resendCmds = append(resendCmds, req)
	}
	c.requestLock.Unlock()

	for _, req := range resendCmds {
		// Stop resending commands if the client disconnected again
		// since the next reconnect will handle them.
		if c.Disconnected() {
			return
		}

		c.marshalAndSend(req.cmd, req.responseChan)
	}
}

// wsReconnectHandler listens for client disconnects and automatically tries
// to reconnect with retry interval that scales based on the number of retries.
// It also resends any commands that had not completed when the client
// disconnected so the disconnect/reconnect process is largely transparent to
// the caller.  This function is not run when the DisableAutoReconnect config
// options is set.
//
// This function must be run as a goroutine.
func (c *Client) wsReconnectHandler() {
out:
	for {
		select {
		case <-c.disconnect:
			// On disconnect, fallthrough to reestablish the
			// connection.

		case <-c.shutdown:
			break out
		}

	reconnect:
		for {
			select {
			case <-c.shutdown:
				break out
			default:
			}

			wsConn, err := dial(c.config)
			if err != nil {
				c.retryCount++
				log.Infof("Failed to connect to %s: %v",
					c.config.Host, err)

				// Scale the retry interval by the number of
				// retries so there is a backoff up to a max
				// of 1 minute.
				scaledInterval := connectionRetryInterval.Nanoseconds() * c.retryCount
				scaledDuration := time.Duration(scaledInterval)
				if scaledDuration > time.Minute {
					scaledDuration = time.Minute
				}
				log.Infof("Retrying connection to %s in "+
					"%s", c.config.Host, scaledDuration)
				time.Sleep(scaledDuration)
				continue reconnect
			}

			log.Infof("Reestablished connection to RPC server %s",
				c.config.Host)

			// Reset the connection state and signal the reconnect
			// has happened.
			c.wsConn = wsConn
			c.retryCount = 0
			c.disconnect = make(chan struct{})

			c.mtx.Lock()
			c.disconnected = false
			c.mtx.Unlock()

			// Start processing input and output for the
			// new connection.
			c.start()

			// Reissue pending commands in another goroutine since
			// the send can block.
			go c.resendCmds()

			// Break out of the reconnect loop back to wait for
			// disconnect again.
			break reconnect
		}
	}
	c.wg.Done()
	log.Tracef("RPC client reconnect handler done for %s", c.config.Host)
}

// handleSendPostMessage handles performing the passed HTTP request, reading the
// result, unmarshalling it, and delivering the unmarhsalled result to the
// provided response channel.
func (c *Client) handleSendPostMessage(details *sendPostDetails) {
	// Post the request.
	cmd := details.command
	log.Tracef("Sending command [%s] with id %d", cmd.Method(), cmd.Id())
	httpResponse, err := c.httpClient.Do(details.request)
	if err != nil {
		details.responseChan <- &futureResult{reply: nil, err: err}
		return
	}

	// Read the raw bytes and close the response.
	respBytes, err := btcjson.GetRaw(httpResponse.Body)
	if err != nil {
		details.responseChan <- &futureResult{reply: nil, err: err}
		return
	}

	// Unmarshal the reply into a concrete result if possible.
	reply, err := btcjson.ReadResultCmd(cmd.Method(), respBytes)
	if err != nil {
		details.responseChan <- &futureResult{reply: nil, err: err}
		return
	}
	details.responseChan <- &futureResult{reply: &reply, err: nil}
}

// sendPostHandler handles all outgoing messages when the client is running
// in HTTP POST mode.  It uses a buffered channel to serialize output messages
// while allowing the sender to continue running asynchronously.  It must be run
// as a goroutine.
func (c *Client) sendPostHandler() {
out:
	for {
		// Send any messages ready for send until the shutdown channel
		// is closed.
		select {
		case details := <-c.sendPostChan:
			c.handleSendPostMessage(details)

		case <-c.shutdown:
			break out
		}
	}

	// Drain any wait channels before exiting so nothing is left waiting
	// around to send.
cleanup:
	for {
		select {
		case details := <-c.sendPostChan:
			details.responseChan <- &futureResult{
				reply: nil,
				err:   ErrClientShutdown,
			}

		default:
			break cleanup
		}
	}
	c.wg.Done()
	log.Tracef("RPC client send handler done for %s", c.config.Host)

}

// sendPostRequest sends the passed HTTP request to the RPC server using the
// HTTP client associated with the client.  It is backed by a buffered channel,
// so it will not block until the send channel is full.
func (c *Client) sendPostRequest(req *http.Request, command btcjson.Cmd, responseChan chan *futureResult) {
	// Don't send the message if shutting down.
	select {
	case <-c.shutdown:
		responseChan <- &futureResult{reply: nil, err: ErrClientShutdown}
	default:
	}

	c.sendPostChan <- &sendPostDetails{
		request:      req,
		command:      command,
		responseChan: responseChan,
	}
}

// newFutureError returns a new future result channel that already has the
// passed error waitin on the channel with the reply set to nil.  This is useful
// to easily return errors from the various Async functions.
func newFutureError(err error) chan *futureResult {
	responseChan := make(chan *futureResult, 1)
	responseChan <- &futureResult{err: err}
	return responseChan
}

// receiveFuture receives from the passed futureResult channel to extract a
// reply or any errors.  The examined errors include an error in the
// futureResult and the error in the reply from the server.  This will block
// until the result is available on the passed channel.
func receiveFuture(responseChan chan *futureResult) (interface{}, error) {
	// Wait for a response on the returned channel.
	response := <-responseChan
	if response.err != nil {
		return nil, response.err
	}

	// At this point, the command was either sent to the server and
	// there is a response from it, or it is intentionally a nil result
	// used to bybass sends for cases such a requesting notifications when
	// there are no handlers.
	reply := response.reply
	if reply == nil {
		return nil, nil
	}

	if reply.Error != nil {
		return nil, reply.Error
	}

	return reply.Result, nil
}

// marshalAndSendPost marshals the passed command to JSON-RPC and sends it to
// the server by issuing an HTTP POST request and returns a response channel
// on which the reply will be delivered.  Typically a new connection is opened
// and closed for each command when using this method, however, the underlying
// HTTP client might coalesce multiple commands depending on several factors
// including the remote server configuration.
func (c *Client) marshalAndSendPost(cmd btcjson.Cmd, responseChan chan *futureResult) {
	marshalledJSON, err := json.Marshal(cmd)
	if err != nil {
		responseChan <- &futureResult{reply: nil, err: err}
		return
	}

	// Generate a request to the configured RPC server.
	protocol := "http"
	if !c.config.DisableTLS {
		protocol = "https"
	}
	url := protocol + "://" + c.config.Host
	req, err := http.NewRequest("POST", url, bytes.NewReader(marshalledJSON))
	if err != nil {
		responseChan <- &futureResult{reply: nil, err: err}
		return
	}
	req.Close = true
	req.Header.Set("Content-Type", "application/json")

	// Configure basic access authorization.
	req.SetBasicAuth(c.config.User, c.config.Pass)

	log.Tracef("Sending command [%s] with id %d", cmd.Method(), cmd.Id())
	c.sendPostRequest(req, cmd, responseChan)
}

// marshalAndSend marshals the passed command to JSON-RPC and sends it to the
// server.  It returns a response channel on which the reply will be delivered.
func (c *Client) marshalAndSend(cmd btcjson.Cmd, responseChan chan *futureResult) {
	marshalledJSON, err := json.Marshal(cmd)
	if err != nil {
		responseChan <- &futureResult{reply: nil, err: err}
		return
	}

	log.Tracef("Sending command [%s] with id %d", cmd.Method(), cmd.Id())
	c.sendMessage(marshalledJSON)
}

// sendCmd sends the passed command to the associated server and returns a
// response channel on which the reply will be deliver at some point in the
// future.  It handles both websocket and HTTP POST mode depending on the
// configuration of the client.
func (c *Client) sendCmd(cmd btcjson.Cmd) chan *futureResult {
	// Choose which marshal and send function to use depending on whether
	// the client running in HTTP POST mode or not.  When running in HTTP
	// POST mode, the command is issued via an HTTP client.  Otherwise,
	// the command is issued via the asynchronous websocket channels.
	responseChan := make(chan *futureResult, 1)
	if c.config.HttpPostMode {
		c.marshalAndSendPost(cmd, responseChan)
		return responseChan
	}

	c.addRequest(cmd.Id().(uint64), &jsonRequest{
		cmd:          cmd,
		responseChan: responseChan,
	})
	c.marshalAndSend(cmd, responseChan)
	return responseChan
}

// sendCmdAndWait sends the passed command to the associated server, waits
// for the reply, and returns the result from it.  It will return the error
// field in the reply if there is one.
func (c *Client) sendCmdAndWait(cmd btcjson.Cmd) (interface{}, error) {
	// Marshal the command to JSON-RPC, send it to the connected server, and
	// wait for a response on the returned channel.
	return receiveFuture(c.sendCmd(cmd))
}

// Disconnected returns whether or not the server is disconnected.
func (c *Client) Disconnected() bool {
	c.mtx.Lock()
	defer c.mtx.Unlock()

	return c.disconnected
}

// Disconnect disconnects the current websocket associated with the client.  The
// connection will automatically be re-established unless the client was
// created with the DisableAutoReconnect flag.
//
// This function has no effect when the client is running in HTTP POST mode.
func (c *Client) Disconnect() {
	if c.config.HttpPostMode {
		return
	}

	c.mtx.Lock()
	defer c.mtx.Unlock()

	// Nothing to do if already disconnected.
	if c.disconnected {
		return
	}

	log.Tracef("Disconnecting RPC client %s", c.config.Host)
	close(c.disconnect)
	c.wsConn.Close()
	c.disconnected = true

	// When operating without auto reconnect, send errors to any pending
	// requests and shutdown the client.
	if c.config.DisableAutoReconnect {
		c.requestLock.Lock()
		for e := c.requestList.Front(); e != nil; e = e.Next() {
			req := e.Value.(*jsonRequest)
			req.responseChan <- &futureResult{
				reply: nil,
				err:   ErrClientDisconnect,
			}
		}
		c.requestLock.Unlock()
		c.removeAllRequests()
		c.Shutdown()
	}
}

// Shutdown shuts down the client by disconnecting any connections associated
// with the client and, when automatic reconnect is enabled, preventing future
// attempts to reconnect.  It also stops all goroutines.
func (c *Client) Shutdown() {
	// Ignore the shutdown request if the client is already in the process
	// of shutting down or already shutdown.
	select {
	case <-c.shutdown:
		return
	default:
	}

	log.Tracef("Shutting down RPC client %s", c.config.Host)
	close(c.shutdown)

	// Send the ErrClientShutdown error to any pending requests.
	c.requestLock.Lock()
	for e := c.requestList.Front(); e != nil; e = e.Next() {
		req := e.Value.(*jsonRequest)
		req.responseChan <- &futureResult{
			reply: nil,
			err:   ErrClientShutdown,
		}
	}
	c.requestLock.Unlock()
	c.removeAllRequests()

	c.Disconnect()
}

// Start begins processing input and output messages.
func (c *Client) start() {
	log.Tracef("Starting RPC client %s", c.config.Host)

	// Start the I/O processing handlers depending on whether the client is
	// in HTTP POST mode or the default websocket mode.
	if c.config.HttpPostMode {
		c.wg.Add(1)
		go c.sendPostHandler()
	} else {
		c.wg.Add(2)
		go c.wsInHandler()
		go c.wsOutHandler()
	}
}

// WaitForShutdown blocks until the client goroutines are stopped and the
// connection is closed.
func (c *Client) WaitForShutdown() {
	c.wg.Wait()
}

// ConnConfig describes the connection configuration parameters for the client.
// This
type ConnConfig struct {
	// Host is the IP address and port of the RPC server you want to connect
	// to.
	Host string

	// Endpoint is the websocket endpoint on the RPC server.  This is
	// typically "ws" or "frontend".
	Endpoint string

	// User is the username to use to authenticate to the RPC server.
	User string

	// Pass is the passphrase to use to authenticate to the RPC server.
	Pass string

	// DisableTLS specifies whether transport layer security should be
	// disabled.  It is recommended to always use TLS if the RPC server
	// supports it as otherwise your username and password is sent across
	// the wire in cleartext.
	DisableTLS bool

	// Certificates are the bytes for a PEM-encoded certificate chain used
	// for the TLS connection.  It has no effect if the DisableTLS parameter
	// is true.
	Certificates []byte

	// Proxy specifies to connect through a SOCKS 5 proxy server.  It may
	// be an empty string if a proxy is not required.
	Proxy string

	// ProxyUser is an optional username to use for the proxy server if it
	// requires authentication.  It has no effect if the Proxy parameter
	// is not set.
	ProxyUser string

	// ProxyPass is an optional password to use for the proxy server if it
	// requires authentication.  It has no effect if the Proxy parameter
	// is not set.
	ProxyPass string

	// DisableAutoReconnect specifies the client should not automatically
	// try to reconnect to the server when it has been disconnected.
	DisableAutoReconnect bool

	// HttpPostMode instructs the client to run using multiple independent
	// connections issuing HTTP POST requests instead of using the default
	// of websockets.  Websockets are generally preferred as some of the
	// features of the client such notifications only work with websockets,
	// however, not all servers support the websocket extensions, so this
	// flag can be set to true to use basic HTTP POST requests instead.
	HttpPostMode bool
}

// newHttpClient returns a new http client that is configured according to the
// proxy and TLS settings in the associated connection configuration.
func newHttpClient(config *ConnConfig) (*http.Client, error) {
	// Set proxy function if there is a proxy configured.
	var proxyFunc func(*http.Request) (*url.URL, error)
	if config.Proxy != "" {
		proxyURL, err := url.Parse(config.Proxy)
		if err != nil {
			return nil, err
		}
		proxyFunc = http.ProxyURL(proxyURL)
	}

	// Configure TLS if needed.
	var tlsConfig *tls.Config
	if !config.DisableTLS {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(config.Certificates)
		tlsConfig = &tls.Config{
			RootCAs: pool,
		}
	}

	client := http.Client{
		Transport: &http.Transport{
			Proxy:           proxyFunc,
			TLSClientConfig: tlsConfig,
		},
	}

	return &client, nil
}

// dial opens a websocket connection using the passed connection configuration
// details.
func dial(config *ConnConfig) (*websocket.Conn, error) {
	// Setup TLS if not disabled.
	var tlsConfig *tls.Config
	var scheme = "ws"
	if !config.DisableTLS {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(config.Certificates)
		tlsConfig = &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}
		scheme = "wss"
	}

	// Create a websocket dialer that will be used to make the connection.
	// It is modified by the proxy setting below as needed.
	dialer := websocket.Dialer{TLSClientConfig: tlsConfig}

	// Setup the proxy if one is configured.
	if config.Proxy != "" {
		proxy := &socks.Proxy{
			Addr:     config.Proxy,
			Username: config.ProxyUser,
			Password: config.ProxyPass,
		}
		dialer.NetDial = proxy.Dial
	}

	// The RPC server requires basic authorization, so create a custom
	// request header with the Authorization header set.
	login := config.User + ":" + config.Pass
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(login))
	requestHeader := make(http.Header)
	requestHeader.Add("Authorization", auth)

	// Dial the connection.
	url := fmt.Sprintf("%s://%s/%s", scheme, config.Host, config.Endpoint)
	wsConn, resp, err := dialer.Dial(url, requestHeader)
	if err != nil {
		if err == websocket.ErrBadHandshake {
			// Detect HTTP authentication error status codes.
			if resp != nil &&
				(resp.StatusCode == http.StatusUnauthorized ||
					resp.StatusCode == http.StatusForbidden) {

				return nil, ErrInvalidAuth
			}

			// The connection was authenticated, but the websocket
			// handshake still failed, so the endpoint is invalid
			// in some way.
			return nil, ErrInvalidEndpoint
		}

		return nil, err
	}
	return wsConn, nil
}

// New creates a new RPC client based on the provided connection configuration
// details.  The notification handlers parameter may be nil if you are not
// interested in receiving notifications and will be ignored when if the
// configuration is set to run in HTTP POST mode.
func New(config *ConnConfig, ntfnHandlers *NotificationHandlers) (*Client, error) {
	// Either open a websocket connection or create an HTTP client depending
	// on the HTTP POST mode.  Also, set the notification handlers to nil
	// when running in HTTP POST mode.
	var wsConn *websocket.Conn
	var httpClient *http.Client
	if config.HttpPostMode {
		ntfnHandlers = nil

		var err error
		httpClient, err = newHttpClient(config)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		wsConn, err = dial(config)
		if err != nil {
			return nil, err
		}
	}

	client := &Client{
		config:       config,
		wsConn:       wsConn,
		httpClient:   httpClient,
		requestMap:   make(map[uint64]*list.Element),
		requestList:  list.New(),
		ntfnHandlers: ntfnHandlers,
		ntfnState:    newNotificationState(),
		sendChan:     make(chan []byte, sendBufferSize),
		sendPostChan: make(chan *sendPostDetails, sendPostBufferSize),
		disconnect:   make(chan struct{}),
		shutdown:     make(chan struct{}),
	}
	client.start()

	if !client.config.HttpPostMode && !client.config.DisableAutoReconnect {
		client.wg.Add(1)
		go client.wsReconnectHandler()
	}

	return client, nil
}
