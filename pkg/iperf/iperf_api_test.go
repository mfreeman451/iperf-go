package iperf

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/op/go-logging"
	//"github.com/gotestyourself/gotest.tools/assert"
	"gotest.tools/assert"
	//"github.com/stretchr/testify/assert"
)

const portServer = 5021
const addrServer = "127.0.0.1:5021"
const addrClient = "127.0.0.1"

var serverTest, clientTest *IperfTest

func init() {
	// Initialize logging
	logging.SetLevel(logging.ERROR, "iperf")
	logging.SetLevel(logging.ERROR, "rudp")

	// Initialize serverTest and clientTest
	serverTest = NewIperfTest()
	clientTest = NewIperfTest()
	serverTest.Init()
	clientTest.Init()

	// Configure serverTest
	serverTest.isServer = true
	serverTest.port = portServer

	// Configure clientTest
	clientTest.isServer = false
	clientTest.port = portServer
	clientTest.addr = addrClient
	clientTest.interval = 1000 // 1000 ms
	clientTest.duration = 5    // 5 s for test
	clientTest.streamNum = 1   // 1 stream only
	clientTest.setTestReverse(false)

	RUDPSetting() // Set protocol and settings

	// Start server in a goroutine and wait for it to be ready
	go func() {
		defer func() {
			if r := recover(); r != nil {
				Log.Errorf("Server goroutine panicked: %v", r)
			}
		}()
		serverTest.runServer()
	}()

	// Wait for server to signal readiness
	select {
	case state := <-serverTest.ctrlChan:
		if state != IPERF_START {
			Log.Errorf("Expected IPERF_START, got %v", state)
			panic("Server failed to start correctly")
		}
	case <-time.After(5 * time.Second):
		Log.Errorf("Server failed to start within 5 seconds")
		panic("Server startup timeout")
	}
	time.Sleep(time.Millisecond * 100) // Additional buffer
}

func TCPSetting() {
	clientTest.setProtocol(TCP_NAME)
	clientTest.noDelay = true
	clientTest.setting.blksize = DEFAULT_TCP_BLKSIZE
	clientTest.setting.burst = false
	clientTest.setting.rate = 1024 * 1024 * 1024 * 1024 // b/s
	clientTest.setting.pacingTime = 100                 //ms
}

func RUDPSetting() {
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

func KCPSetting() {
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

func RecvCheckState(t *testing.T, state int) int {
	buf := make([]byte, 4)
	if n, err := clientTest.ctrlConn.Read(buf); err == nil {
		s := binary.LittleEndian.Uint32(buf[:])
		Log.Debugf("Ctrl conn receive n = %v state = [%v]", n, s)
		//s, err := strconv.Atoi(string(buf[:n]))
		if s != uint32(state) {
			Log.Errorf("recv state[%v] != expected state[%v]", s, state)
			t.FailNow()
			return -1
		}
		clientTest.state = uint(state)
		Log.Infof("Client Enter %v state", clientTest.state)
	}
	return 0
}

func CreateStreams(t *testing.T) int {
	if rtn := clientTest.createStreams(); rtn < 0 {
		Log.Errorf("create_streams failed. rtn = %v", rtn)
		return -1
	}
	// check client state
	assert.Equal(t, uint(len(clientTest.streams)), clientTest.streamNum)
	for _, sp := range clientTest.streams {
		assert.Equal(t, sp.test, clientTest)
		if clientTest.mode == IPERF_SENDER {
			assert.Equal(t, sp.role, SENDER_STREAM)
		} else {
			assert.Equal(t, sp.role, RECEIVER_STREAM)
		}
		assert.Assert(t, sp.result != nil)
		assert.Equal(t, sp.canSend, false) // set true after create_send_timer
		assert.Assert(t, sp.conn != nil)
		assert.Assert(t, sp.sendTicker.ticker == nil) // ticker haven't been created yet
	}
	time.Sleep(time.Millisecond * 10) // ensure server side has created all the streams
	// check server state
	assert.Equal(t, uint(len(serverTest.streams)), clientTest.streamNum)

	for _, sp := range serverTest.streams {
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
	}

	return 0
}

func handleTestStart(t *testing.T) int {
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

	// check client
	for _, sp := range clientTest.streams {
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
	}

	// check server, should finish test_start process and enter test_running now
	for _, sp := range serverTest.streams {
		assert.Assert(t, sp.result.start_time.Before(time.Now().Add(time.Duration(time.Millisecond))))
		assert.Assert(t, sp.test.timer.timer != nil)
		assert.Assert(t, sp.test.statsTicker.ticker != nil)
		assert.Assert(t, sp.test.reportTicker.ticker != nil)
		assert.Equal(t, sp.test.state, uint(TEST_RUNNING))
	}

	return 0
}

func handleTestRunning(t *testing.T) int {
	Log.Info("Client enter Test Running state...")
	for i, sp := range clientTest.streams {
		if clientTest.mode == IPERF_SENDER {
			go sp.iperfSend(clientTest)
			Log.Infof("Stream %v start sending.", i)
		} else {
			go sp.iperfRecv(clientTest)
			Log.Infof("Stream %v start receiving.", i)
		}
	}

	Log.Info("Client all Stream start. Wait for finish...")
	// wait for send/write end (triggered by timer)
	//for {
	//	if client_test.done {
	//		time.Sleep(time.Millisecond)
	//		break
	//	}
	//}

	for i := 0; i < int(clientTest.streamNum); i++ {
		s := <-clientTest.ctrlChan
		assert.Equal(t, s, uint(TEST_END))
	}

	Log.Infof("Client All Send Stream closed.")

	clientTest.done = true

	if clientTest.statsCallback != nil {
		clientTest.statsCallback(clientTest)
	}

	if clientTest.setSendState(TEST_END) < 0 {
		Log.Errorf("set_send_state failed. %v", TEST_END)

		t.FailNow()
	}

	// check client
	assert.Equal(t, clientTest.done, true)
	assert.Assert(t, clientTest.timer.timer == nil)
	assert.Equal(t, clientTest.state, uint(TEST_END))

	var totalBytes uint64

	for _, sp := range clientTest.streams {
		if clientTest.mode == IPERF_SENDER {
			totalBytes += sp.result.bytes_sent
		} else {
			totalBytes += sp.result.bytes_received
		}
	}

	if clientTest.mode == IPERF_SENDER {
		assert.Equal(t, clientTest.bytesSent, totalBytes)
		assert.Equal(t, clientTest.bytesReceived, uint64(0))
	} else {
		assert.Equal(t, clientTest.bytesReceived, totalBytes)
		assert.Equal(t, clientTest.bytesSent, uint64(0))
	}

	time.Sleep(time.Millisecond * 10) // ensure server change state

	// check server
	assert.Equal(t, serverTest.done, true)
	assert.Equal(t, serverTest.state, uint(IPERF_EXCHANGE_RESULT))

	absoluteBytesDiff := int64(serverTest.bytesReceived) - int64(clientTest.bytesSent)
	if absoluteBytesDiff < 0 {
		absoluteBytesDiff = 0 - absoluteBytesDiff
	}

	if float64(absoluteBytesDiff)/float64(clientTest.bytesSent) > 0.01 { // if bytes difference larger than 1%
		t.FailNow()
	}

	//assert.Equal(t, server_test.bytes_received, client_test.bytes_sent)
	//assert.Equal(t, server_test.blocks_received, client_test.blocks_sent)		// block num not always same
	totalBytes = 0
	for _, sp := range serverTest.streams {
		if serverTest.mode == IPERF_SENDER {
			totalBytes += sp.result.bytes_sent
		} else {
			totalBytes += sp.result.bytes_received
		}
	}

	if serverTest.mode == IPERF_SENDER {
		assert.Equal(t, serverTest.bytesSent, totalBytes)
		assert.Equal(t, serverTest.bytesReceived, uint64(0))
	} else {
		assert.Equal(t, serverTest.bytesReceived, totalBytes)
		assert.Equal(t, serverTest.bytesSent, uint64(0))
	}
	return 0
}

func handleExchangeResult(t *testing.T) int {
	if rtn := clientTest.exchangeResults(); rtn < 0 {
		Log.Errorf("exchange_results failed. rtn = %v", rtn)

		return -1
	}

	// check client
	assert.Equal(t, clientTest.done, true)

	for i, sp := range clientTest.streams {
		ssp := serverTest.streams[i]

		assert.Equal(t, sp.result.bytes_received, ssp.result.bytes_received)
		assert.Equal(t, sp.result.bytes_sent, ssp.result.bytes_sent)
	}

	// check server
	assert.Equal(t, serverTest.state, uint(IPERF_DISPLAY_RESULT))

	return 0
}

/*
	Test case can only be run one by one
*/

/*
func TestCtrlConnect(t *testing.T){
	if rtn := client_test.ConnectServer(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_EXCHANGE_PARAMS)
	if err := client_test.ctrl_conn.Close(); err != nil {
		log.Errorf("close ctrl_conn failed.")
		t.FailNow()
	}
	if err := server_test.ctrl_conn.Close(); err != nil {
		log.Errorf("close ctrl_conn failed.")
		t.FailNow()
	}
}

func TestExchangeParams(t *testing.T){
	if rtn := client_test.ConnectServer(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_EXCHANGE_PARAMS)
	if rtn := client_test.exchange_params(); rtn < 0 {
		t.FailNow()
	}

	time.Sleep(time.Second)
	assert.Equal(t, server_test.proto.name(), client_test.proto.name())
	assert.Equal(t, server_test.stream_num, client_test.stream_num)
	assert.Equal(t, server_test.duration, client_test.duration)
	assert.Equal(t, server_test.interval, client_test.interval)
	assert.Equal(t, server_test.no_delay, client_test.no_delay)
}

func TestCreateOneStream(t *testing.T){
	// create only one stream
	if rtn := client_test.ConnectServer(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_EXCHANGE_PARAMS)
	if rtn := client_test.exchange_params(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_CREATE_STREAM)
	CreateStreams(t)
}

func TestCreateMultiStreams(t *testing.T){
	// create multi streams
	if rtn := client_test.ConnectServer(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_EXCHANGE_PARAMS)
	client_test.stream_num = 5	// change stream_num before exchange params
	if rtn := client_test.exchange_params(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_CREATE_STREAM)
	if rtn := CreateStreams(t); rtn < 0{
		t.FailNow()
	}
}

func TestTestStart(t *testing.T){
	if rtn := client_test.ConnectServer(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_EXCHANGE_PARAMS)
	if rtn := client_test.exchange_params(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_CREATE_STREAM)
	if rtn := CreateStreams(t); rtn < 0{
		t.FailNow()
	}
	RecvCheckState(t, TEST_START)
	if rtn := handleTestStart(t); rtn < 0{
		t.FailNow()
	}
	RecvCheckState(t, TEST_RUNNING)
}

func TestTestRunning(t *testing.T){
	if rtn := client_test.ConnectServer(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_EXCHANGE_PARAMS)
	client_test.stream_num = 2
	if rtn := client_test.exchange_params(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_CREATE_STREAM)
	if rtn := CreateStreams(t); rtn < 0{
		t.FailNow()
	}
	RecvCheckState(t, TEST_START)
	if rtn := handleTestStart(t); rtn < 0{
		t.FailNow()
	}
	RecvCheckState(t, TEST_RUNNING)
	if handleTestRunning(t) < 0{
		t.FailNow()
	}
	RecvCheckState(t, IPERF_EXCHANGE_RESULT)
}

func TestExchangeResult(t *testing.T){
	if rtn := client_test.ConnectServer(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_EXCHANGE_PARAMS)
	client_test.stream_num = 2
	if rtn := client_test.exchange_params(); rtn < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_CREATE_STREAM)
	if rtn := CreateStreams(t); rtn < 0{
		t.FailNow()
	}
	RecvCheckState(t, TEST_START)
	if rtn := handleTestStart(t); rtn < 0{
		t.FailNow()
	}
	RecvCheckState(t, TEST_RUNNING)
	if handleTestRunning(t) < 0{
		t.FailNow()
	}
	RecvCheckState(t, IPERF_EXCHANGE_RESULT)
	if handleExchangeResult(t) < 0 {
		t.FailNow()
	}
	RecvCheckState(t, IPERF_DISPLAY_RESULT)
}
*/

func TestDisplayResult(t *testing.T) {
	defer func() {
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

	if rtn := clientTest.ConnectServer(); rtn < 0 {
		t.FailNow()
	}

	RecvCheckState(t, IPERF_EXCHANGE_PARAMS)
	//client_test.stream_num = 2
	if rtn := clientTest.exchangeParams(); rtn < 0 {
		t.FailNow()
	}

	RecvCheckState(t, IPERF_CREATE_STREAM)
	if rtn := CreateStreams(t); rtn < 0 {
		t.FailNow()
	}

	RecvCheckState(t, TEST_START)
	if rtn := handleTestStart(t); rtn < 0 {
		t.FailNow()
	}

	RecvCheckState(t, TEST_RUNNING)
	if handleTestRunning(t) < 0 {
		t.FailNow()
	}

	RecvCheckState(t, IPERF_EXCHANGE_RESULT)
	if handleExchangeResult(t) < 0 {
		t.FailNow()
	}

	RecvCheckState(t, IPERF_DISPLAY_RESULT)

	clientTest.clientEnd()

	time.Sleep(time.Millisecond * 10) // wait for server
	assert.Equal(t, clientTest.state, uint(IPERF_DONE))
	assert.Equal(t, serverTest.state, uint(IPERF_DONE))
	// check output with your own eyes

	time.Sleep(time.Second * 5) // wait for server
}
