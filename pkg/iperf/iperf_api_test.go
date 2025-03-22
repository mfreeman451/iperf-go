package iperf

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/op/go-logging"
	"gotest.tools/assert"
)

const portServer = 5021
const addrServer = "127.0.0.1:5021"
const addrClient = "127.0.0.1"

// SetupServer starts the server and returns a WaitGroup to signal when streams are ready.
func SetupServer(t *testing.T) (*IperfTest, *sync.WaitGroup) {
	serverTest := NewIperfTest()
	serverTest.Init()
	serverTest.isServer = true
	serverTest.port = portServer

	wg := &sync.WaitGroup{}
	wg.Add(1) // Wait for server to create streams

	go func() {
		defer func() {
			if r := recover(); r != nil {
				Log.Errorf("Server goroutine panicked: %v", r)
			}
			wg.Done() // Ensure wg.Done() is called even on panic
		}()
		Log.Infof("Starting server on port %d", portServer)
		if err := serverTest.runServer(wg); err != 0 {
			Log.Errorf("Server failed with error code: %d", err)
		}
		Log.Infof("Server runServer completed")
	}()

	// Wait for server to signal IPERF_START
	select {
	case state := <-serverTest.ctrlChan:
		if state != IPERF_START {
			Log.Errorf("Expected IPERF_START, got %v", state)
			t.Fatalf("Server failed to start correctly, got state %v", state)
		}
		Log.Infof("Server signaled IPERF_START")
	case <-time.After(5 * time.Second):
		Log.Errorf("Server failed to start within 5 seconds")
		t.Fatalf("Server startup timeout")
	}

	return serverTest, wg
}

func init() {
	// Initialize logging only once
	logging.SetLevel(logging.ERROR, "iperf")
	logging.SetLevel(logging.ERROR, "rudp")
}

func TCPSetting(clientTest *IperfTest) {
	clientTest.setProtocol(TCP_NAME)
	clientTest.noDelay = true
	clientTest.setting.blksize = DEFAULT_TCP_BLKSIZE
	clientTest.setting.burst = false
	clientTest.setting.rate = 1024 * 1024 * 1024 * 1024 // b/s
	clientTest.setting.pacingTime = 100                 // ms
}

func RUDPSetting(clientTest *IperfTest) {
	clientTest.setProtocol(RUDP_NAME)
	clientTest.noDelay = false
	clientTest.setting.blksize = DEFAULT_RUDP_BLKSIZE
	clientTest.setting.burst = true
	clientTest.setting.noCong = false // false for BBR control
	clientTest.setting.sndWnd = 10
	clientTest.setting.rcvWnd = 1024
	clientTest.setting.readBufSize = DEFAULT_READ_BUF_SIZE
	clientTest.setting.writeBufSize = DEFAULT_WRITE_BUF_SIZE
	clientTest.setting.flushInterval = DEFAULT_FLUSH_INTERVAL
	clientTest.setting.dataShards = 3
	clientTest.setting.parityShards = 1
}

func KCPSetting(clientTest *IperfTest) {
	clientTest.setProtocol(KCP_NAME)
	clientTest.noDelay = false
	clientTest.setting.blksize = DEFAULT_RUDP_BLKSIZE
	clientTest.setting.burst = true
	clientTest.setting.noCong = true // false for BBR control
	clientTest.setting.sndWnd = 512
	clientTest.setting.rcvWnd = 1024
	clientTest.setting.readBufSize = DEFAULT_READ_BUF_SIZE
	clientTest.setting.writeBufSize = DEFAULT_WRITE_BUF_SIZE
	clientTest.setting.flushInterval = DEFAULT_FLUSH_INTERVAL
}

func RecvCheckState(t *testing.T, state int, clientTest *IperfTest) int {
	t.Helper()

	buf := make([]byte, 4)
	if n, err := clientTest.ctrlConn.Read(buf); err == nil {
		s := binary.LittleEndian.Uint32(buf[:])
		Log.Debugf("Ctrl conn receive n = %v state = [%v]", n, s)
		if s != uint32(state) {
			Log.Errorf("recv state[%v] != expected state[%v]", s, state)
			t.FailNow()
			return -1
		}
		clientTest.mu.Lock()
		clientTest.state = uint(state)
		clientTest.mu.Unlock()
		Log.Infof("Client Enter %v state", clientTest.state)
	} else {
		Log.Errorf("Ctrl conn read failed: %v", err)
		return -1
	}
	return 0
}

func CreateStreams(t *testing.T, clientTest, serverTest *IperfTest) int {
	t.Helper()

	if rtn := clientTest.createStreams(); rtn < 0 {
		Log.Errorf("create_streams failed. rtn = %v", rtn)
		return -1
	}
	clientTest.mu.Lock()
	defer clientTest.mu.Unlock()

	// Check client state
	assert.Equal(t, uint(len(clientTest.streams)), clientTest.streamNum)
	for _, sp := range clientTest.streams {
		sp.mu.Lock()
		assert.Equal(t, sp.test, clientTest)
		if clientTest.mode == IPERF_SENDER {
			assert.Equal(t, sp.role, SENDER_STREAM)
		} else {
			assert.Equal(t, sp.role, RECEIVER_STREAM)
		}
		assert.Assert(t, sp.result != nil)
		assert.Equal(t, sp.canSend, false) // set true after create_send_timer
		assert.Assert(t, sp.conn != nil)
		assert.Assert(t, sp.sendTicker.ticker == nil) // ticker hasn't been created yet
		sp.mu.Unlock()
	}
	time.Sleep(time.Millisecond * 10) // ensure server side has created all the streams

	// Check server state
	serverTest.mu.Lock()
	defer serverTest.mu.Unlock()
	assert.Equal(t, uint(len(serverTest.streams)), clientTest.streamNum)
	for _, sp := range serverTest.streams {
		sp.mu.Lock()
		assert.Equal(t, sp.test, serverTest)
		if serverTest.mode == IPERF_SENDER {
			assert.Equal(t, sp.role, SENDER_STREAM)
		} else {
			assert.Equal(t, sp.role, RECEIVER_STREAM)
		}
		assert.Assert(t, sp.result != nil)
		if serverTest.mode == IPERF_SENDER {
			assert.Equal(t, sp.canSend, true)
			if clientTest.setting.burst == true {
				assert.Assert(t, sp.sendTicker.ticker == nil)
			} else {
				assert.Assert(t, sp.sendTicker.ticker != nil)
			}
		} else {
			assert.Equal(t, sp.canSend, false)
			assert.Assert(t, sp.sendTicker.ticker == nil)
		}
		assert.Assert(t, sp.conn != nil)
		sp.mu.Unlock()
	}
	return 0
}

func handleTestStart(t *testing.T, clientTest, serverTest *IperfTest) int {
	t.Helper()

	if rtn := clientTest.initTest(); rtn < 0 {
		Log.Errorf("init_test failed. rtn = %v", rtn)
		return -1
	}
	if rtn := clientTest.createClientTimer(); rtn < 0 {
		Log.Errorf("create_client_timer failed. rtn = %v", rtn)
		return -1
	}
	if rtn := clientTest.createClientOmitTimer(); rtn < 0 {
		Log.Errorf("create_client_omit_timer failed. rtn = %v", rtn)
		return -1
	}
	if clientTest.mode == IPERF_SENDER {
		if rtn := clientTest.createSenderTicker(); rtn < 0 {
			Log.Errorf("create_client_send_timer failed. rtn = %v", rtn)
			return -1
		}
	}

	// Check client
	clientTest.mu.Lock()
	for _, sp := range clientTest.streams {
		sp.mu.Lock()
		assert.Assert(t, sp.result.start_time.Before(time.Now().Add(time.Duration(time.Millisecond))))
		assert.Assert(t, sp.test.timer.timer != nil)
		assert.Assert(t, sp.test.statsTicker.ticker != nil)
		assert.Assert(t, sp.test.reportTicker.ticker != nil)
		if clientTest.mode == IPERF_SENDER {
			assert.Equal(t, sp.canSend, true)
			if clientTest.setting.burst == true {
				assert.Assert(t, sp.sendTicker.ticker == nil)
			} else {
				assert.Assert(t, sp.sendTicker.ticker != nil)
			}
		} else {
			assert.Equal(t, sp.canSend, false)
			assert.Assert(t, sp.sendTicker.ticker == nil)
		}
		sp.mu.Unlock()
	}
	clientTest.mu.Unlock()

	// Check server
	serverTest.mu.Lock()
	for _, sp := range serverTest.streams {
		sp.mu.Lock()
		assert.Assert(t, sp.result.start_time.Before(time.Now().Add(time.Duration(time.Millisecond))))
		assert.Assert(t, sp.test.timer.timer != nil)
		assert.Assert(t, sp.test.statsTicker.ticker != nil)
		assert.Assert(t, sp.test.reportTicker.ticker != nil)
		assert.Equal(t, sp.test.state, uint(TEST_RUNNING))
		sp.mu.Unlock()
	}
	serverTest.mu.Unlock()

	return 0
}

func handleTestRunning(t *testing.T, clientTest, serverTest *IperfTest) int {
	Log.Info("Client enter Test Running state...")
	var wg sync.WaitGroup
	for i, sp := range clientTest.streams {
		wg.Add(1)
		if clientTest.mode == IPERF_SENDER {
			go func(i int, sp *iperfStream) {
				defer wg.Done()
				sp.iperfSend(clientTest)
				Log.Infof("Stream %v finished sending.", i)
			}(i, sp)
		} else {
			go func(i int, sp *iperfStream) {
				defer wg.Done()
				sp.iperfRecv(clientTest)
				Log.Infof("Stream %v finished receiving.", i)
			}(i, sp)
		}
	}
	Log.Info("Client all Stream start. Waiting for finish...")
	wg.Wait()

	Log.Infof("Client All Streams closed.")
	clientTest.mu.Lock()
	clientTest.done = true
	clientTest.mu.Unlock()

	if clientTest.statsCallback != nil {
		clientTest.statsCallback(clientTest)
	}
	if clientTest.setSendState(TEST_END) < 0 {
		Log.Errorf("set_send_state failed. %v", TEST_END)
		t.FailNow()
	}

	// Check client
	clientTest.mu.Lock()
	assert.Equal(t, clientTest.done, true)
	assert.Assert(t, clientTest.timer.timer == nil)
	assert.Equal(t, clientTest.state, uint(TEST_END))
	var totalBytes uint64
	for _, sp := range clientTest.streams {
		sp.mu.Lock()
		if clientTest.mode == IPERF_SENDER {
			totalBytes += sp.result.bytes_sent
		} else {
			totalBytes += sp.result.bytes_received
		}
		sp.mu.Unlock()
	}
	if clientTest.mode == IPERF_SENDER {
		assert.Equal(t, clientTest.bytesSent, totalBytes)
		assert.Equal(t, clientTest.bytesReceived, uint64(0))
	} else {
		assert.Equal(t, clientTest.bytesReceived, totalBytes)
		assert.Equal(t, clientTest.bytesSent, uint64(0))
	}
	clientTest.mu.Unlock()

	time.Sleep(time.Millisecond * 10) // ensure server changes state

	// Check server
	serverTest.mu.Lock()
	assert.Equal(t, serverTest.done, true)
	assert.Equal(t, serverTest.state, uint(IPERF_EXCHANGE_RESULT))
	var serverTotalBytes uint64
	for _, sp := range serverTest.streams {
		sp.mu.Lock()
		if serverTest.mode == IPERF_SENDER {
			serverTotalBytes += sp.result.bytes_sent
		} else {
			serverTotalBytes += sp.result.bytes_received
		}
		sp.mu.Unlock()
	}
	if serverTest.mode == IPERF_SENDER {
		assert.Equal(t, serverTest.bytesSent, serverTotalBytes)
		assert.Equal(t, serverTest.bytesReceived, uint64(0))
	} else {
		assert.Equal(t, serverTest.bytesReceived, serverTotalBytes)
		assert.Equal(t, serverTest.bytesSent, uint64(0))
	}
	absoluteBytesDiff := int64(serverTest.bytesReceived) - int64(clientTest.bytesSent)
	if absoluteBytesDiff < 0 {
		absoluteBytesDiff = 0 - absoluteBytesDiff
	}
	if clientTest.bytesSent > 0 && float64(absoluteBytesDiff)/float64(clientTest.bytesSent) > 0.01 {
		t.Errorf("Bytes difference exceeds 1%%: server received %v, client sent %v", serverTest.bytesReceived, clientTest.bytesSent)
	}
	serverTest.mu.Unlock()

	return 0
}

func handleExchangeResult(t *testing.T, clientTest, serverTest *IperfTest) int {
	t.Helper()

	if rtn := clientTest.exchangeResults(); rtn < 0 {
		Log.Errorf("exchange_results failed. rtn = %v", rtn)
		return -1
	}

	// Check client
	clientTest.mu.Lock()
	assert.Equal(t, clientTest.done, true)
	for i, sp := range clientTest.streams {
		sp.mu.Lock()
		ssp := serverTest.streams[i]
		ssp.mu.Lock()
		assert.Equal(t, sp.result.bytes_received, ssp.result.bytes_received)
		assert.Equal(t, sp.result.bytes_sent, ssp.result.bytes_sent)
		ssp.mu.Unlock()
		sp.mu.Unlock()
	}
	clientTest.mu.Unlock()

	// Check server
	serverTest.mu.Lock()
	assert.Equal(t, serverTest.state, uint(IPERF_DISPLAY_RESULT))
	serverTest.mu.Unlock()

	return 0
}

func TestDisplayResult(t *testing.T) {
	clientTest := NewIperfTest()
	clientTest.Init()
	clientTest.isServer = false
	clientTest.port = portServer
	clientTest.addr = addrClient
	clientTest.interval = 1000
	clientTest.duration = 5
	clientTest.streamNum = 1
	clientTest.setTestReverse(false)
	RUDPSetting(clientTest)

	serverTest, wg := SetupServer(t)
	defer func() {
		Log.Infof("Test cleanup: closing connections")
		serverTest.closeAllStreams()
		clientTest.closeAllStreams()
		if serverTest.ctrlConn != nil {
			if err := serverTest.ctrlConn.Close(); err != nil {
				t.Logf("Failed to close server ctrlConn: %v", err)
			}
		}
		if clientTest.ctrlConn != nil {
			if err := clientTest.ctrlConn.Close(); err != nil {
				t.Logf("Failed to close client ctrlConn: %v", err)
			}
		}
	}()

	Log.Infof("Client connecting to server")
	if rtn := clientTest.ConnectServer(); rtn < 0 {
		t.Fatalf("Client failed to connect: %d", rtn)
	}
	Log.Infof("Client connected successfully")
	clientTest.ctrlConn.SetDeadline(time.Now().Add(30 * time.Second))

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		Log.Infof("Starting client logic")
		test := clientTest
		go test.handleClientCtrlMsg()

		Log.Infof("Client sending params")
		if rtn := test.exchangeParams(); rtn < 0 {
			Log.Errorf("Client exchangeParams failed: %d", rtn)
			return
		}
		Log.Infof("Client params sent")

		for {
			test.mu.Lock()
			currentState := test.state
			test.mu.Unlock()
			if currentState == IPERF_DONE {
				return
			}

			select {
			case state := <-test.ctrlChan:
				Log.Debugf("Client received state %v", state)
				test.mu.Lock()
				if state == IPERF_DONE {
					test.mu.Unlock()
					return
				}
				test.mu.Unlock()
			case <-time.After(20 * time.Second):
				Log.Errorf("Client state loop timed out")
				return
			}
		}
	}()

	waitChan := make(chan struct{})
	go func() {
		wg.Wait()
		Log.Infof("Server streams created")
		close(waitChan)
	}()
	select {
	case <-waitChan:
		Log.Infof("Server streams ready")
	case <-time.After(15 * time.Second):
		Log.Errorf("Server failed to create streams within 15 seconds")
		t.Fatalf("Server failed to create streams within 15 seconds")
	}

	RecvCheckState(t, IPERF_CREATE_STREAM, clientTest)
	if rtn := CreateStreams(t, clientTest, serverTest); rtn < 0 {
		t.Fatalf("CreateStreams failed: %d", rtn)
	}
	RecvCheckState(t, TEST_START, clientTest)
	if rtn := handleTestStart(t, clientTest, serverTest); rtn < 0 {
		t.Fatalf("handleTestStart failed: %d", rtn)
	}
	RecvCheckState(t, TEST_RUNNING, clientTest)
	if rtn := handleTestRunning(t, clientTest, serverTest); rtn < 0 {
		t.Fatalf("handleTestRunning failed: %d", rtn)
	}
	RecvCheckState(t, IPERF_EXCHANGE_RESULT, clientTest)
	if rtn := handleExchangeResult(t, clientTest, serverTest); rtn < 0 {
		t.Fatalf("handleExchangeResult failed: %d", rtn)
	}
	RecvCheckState(t, IPERF_DISPLAY_RESULT, clientTest)

	done := make(chan struct{})
	go func() {
		clientTest.clientEnd()
		close(done)
	}()
	select {
	case <-done:
		clientTest.mu.Lock()
		serverTest.mu.Lock()
		assert.Equal(t, clientTest.state, uint(IPERF_DONE))
		assert.Equal(t, serverTest.state, uint(IPERF_DONE))
		serverTest.mu.Unlock()
		clientTest.mu.Unlock()
	case <-time.After(10 * time.Second):
		t.Fatal("Test timed out after 10 seconds")
	}
	<-clientDone
}
