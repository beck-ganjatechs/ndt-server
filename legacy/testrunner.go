package legacy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/m-lab/ndt-server/legacy/c2s"
	"github.com/m-lab/ndt-server/legacy/metrics"
	"github.com/m-lab/ndt-server/legacy/protocol"
	"github.com/m-lab/ndt-server/legacy/s2c"
	"github.com/m-lab/ndt-server/legacy/testresponder"
)

const (
	cTestC2S    = 2
	cTestS2C    = 4
	cTestStatus = 16
)

// BasicServer contains everything needed to start a new server on a random port.
type BasicServer struct {
	CertFile   string
	KeyFile    string
	ServerType testresponder.ServerType
	HTTPAddr   string
}

// TODO: run meta test.
func runMetaTest(ws protocol.Connection) {
	var err error
	var message *protocol.JSONMessage

	protocol.SendJSONMessage(protocol.TestPrepare, "", ws)
	protocol.SendJSONMessage(protocol.TestStart, "", ws)
	for {
		message, err = protocol.ReceiveJSONMessage(ws, protocol.TestMsg)
		if message.Msg == "" || err != nil {
			break
		}
		log.Println("Meta message: ", message)
	}
	if err != nil {
		log.Println("Error reading JSON message:", err)
		return
	}
	protocol.SendJSONMessage(protocol.TestFinalize, "", ws)
}

// ServeHTTP is the command channel for the NDT-WS or NDT-WSS test. All
// subsequent client communication is synchronized with this method. Returning
// closes the websocket connection, so only occurs after all tests complete or
// an unrecoverable error. It is called ServeHTTP to make sure that the Server
// implements the http.Handler interface.
func (s *BasicServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upgrader := testresponder.MakeNdtUpgrader([]string{"ndt"})
	wsc, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("ERROR SERVER:", err)
		return
	}
	ws := protocol.AdaptWsConn(wsc)
	defer ws.Close()
	s.HandleControlChannel(ws)
}

func (s *BasicServer) HandleControlChannel(conn protocol.Connection) {
	config := &testresponder.Config{
		ServerType: s.ServerType,
		CertFile:   s.CertFile,
		KeyFile:    s.KeyFile,
	}

	message, err := protocol.ReceiveJSONMessage(conn, protocol.MsgExtendedLogin)
	if err != nil {
		log.Println("Error reading JSON message:", err)
		return
	}
	tests, err := strconv.ParseInt(message.Tests, 10, 64)
	if err != nil {
		log.Println("Failed to parse Tests integer:", err)
		return
	}
	if (tests & cTestStatus) == 0 {
		log.Println("We don't support clients that don't support TestStatus")
		return
	}
	testsToRun := []string{}
	runC2s := (tests & cTestC2S) != 0
	runS2c := (tests & cTestS2C) != 0

	if runC2s {
		testsToRun = append(testsToRun, strconv.Itoa(cTestC2S))
	}
	if runS2c {
		testsToRun = append(testsToRun, strconv.Itoa(cTestS2C))
	}

	protocol.SendJSONMessage(protocol.SrvQueue, "0", conn)
	protocol.SendJSONMessage(protocol.MsgLogin, "v5.0-NDTinGO", conn)
	protocol.SendJSONMessage(protocol.MsgLogin, strings.Join(testsToRun, " "), conn)

	var c2sRate, s2cRate float64
	if runC2s {
		c2sRate, err = c2s.ManageTest(conn, config)
		if err != nil {
			log.Println("ERROR: manageC2sTest", err)
		} else {
			metrics.TestRate.WithLabelValues("c2s").Observe(c2sRate / 1000.0)
		}
	}
	if runS2c {
		s2cRate, err = s2c.ManageTest(conn, config)
		if err != nil {
			log.Println("ERROR: manageS2cTest", err)
		} else {
			metrics.TestRate.WithLabelValues("s2c").Observe(s2cRate / 1000.0)
		}
	}
	log.Printf("NDT: uploaded at %.4f and downloaded at %.4f", c2sRate, s2cRate)
	protocol.SendJSONMessage(protocol.MsgResults, fmt.Sprintf("You uploaded at %.4f and downloaded at %.4f", c2sRate, s2cRate), conn)
	protocol.SendJSONMessage(protocol.MsgLogout, "", conn)

}

func (s *BasicServer) SniffThenHandle(conn net.Conn) {
	// Peek at the first three bytes. If they are "GET", then this is an HTTP
	// conversation and should be forwarded to the HTTP server.
	input := bufio.NewReader(conn)
	lead, err := input.Peek(3)
	if err != nil {
		log.Println("Could not handle connection", conn)
		return
	}
	if string(lead) == "GET" {
		// Forward HTTP-like handshakes to the HTTP server. Note that this does NOT
		// introduce overhead for the s2c and c2s tests, because in those tests the
		// HTTP server itself opens the testing port, and that server will not use
		// this TCP proxy.
		fwd, err := net.Dial("tcp", s.HTTPAddr)
		if err != nil {
			log.Println("Could not forward connection", err)
			return
		}
		wg := sync.WaitGroup{}
		wg.Add(2)
		// Copy the input channel.
		go func() {
			io.Copy(fwd, input)
			wg.Done()
		}()
		// Copy the ouput channel.
		go func() {
			io.Copy(conn, fwd)
			wg.Done()
		}()
		// When both Copy calls are done, close everything.
		go func() {
			wg.Wait()
			conn.Close()
			fwd.Close()
		}()
		return
	}
	// If there was no error and there was no GET, then this should be treated as a
	// legitimate attempt to perform a non-ws-based NDT test.

	// First, send the kickoff message (which is only sent for non-WS clients),
	// then transition to the protocol engine where everything should be the same
	// for TCP, WS, and WSS.
	kickoff := "123456 654321"
	n, err := conn.Write([]byte(kickoff))
	if n != len(kickoff) || err != nil {
		log.Printf("Could not write %d byte kickoff string: %d bytes written err: %v\n", len(kickoff), n, err)
	}
	s.HandleControlChannel(protocol.AdaptNetConn(conn, input))
}

func (s *BasicServer) ListenAndServeRawAsync(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// Close the listener when the context is canceled. We do this in a separate
	// goroutine to ensure that context cancellation interrupts the Accept() call.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	// Serve requests until the context is canceled.
	go func() {
		for ctx.Err() == nil {
			conn, err := ln.Accept()
			if err != nil {
				log.Println("Failed to accept connection:", err)
				continue
			}
			go s.SniffThenHandle(conn)
		}
	}()
	return nil
}
