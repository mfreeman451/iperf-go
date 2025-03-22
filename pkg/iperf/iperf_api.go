package iperf

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/op/go-logging"
)

func NewIperfTest() (test *IperfTest) {
	test = new(IperfTest)

	test.ctrlChan = make(chan uint, 5)
	test.setting = new(iperfSetting)
	test.reporterCallback = iperfReporterCallback
	test.statsCallback = iperfStatsCallback
	test.chStats = make(chan bool, 1)

	return
}

func (test *IperfTest) setProtocol(protoName string) int {
	for _, proto := range test.protocols {
		if protoName == proto.name() {
			test.proto = proto

			return 0
		}
	}

	return -1
}

const ACK_SIGNAL = 0x41434B41 // Unique ACK value, ASCII "ACKA"

func (test *IperfTest) setSendState(state uint) int {
	validStates := map[uint]bool{
		IPERF_START: true, IPERF_DONE: true, IPERF_CREATE_STREAM: true,
		IPERF_EXCHANGE_PARAMS: true, IPERF_EXCHANGE_RESULT: true, IPERF_DISPLAY_RESULT: true,
		TEST_START: true, TEST_RUNNING: true, TEST_END: true,
		CLIENT_TERMINATE: true, SERVER_TERMINATE: true,
	}
	if !validStates[state] {
		Log.Errorf("Invalid state value: %v", state)
		return -1
	}

	test.mu.Lock()
	test.state = state
	test.ctrlChan <- state
	test.mu.Unlock()

	// Send state to peer
	bs := make([]byte, 4)
	binary.LittleEndian.PutUint32(bs, uint32(state))
	n, err := test.ctrlConn.Write(bs)
	if err != nil {
		Log.Errorf("Write state error: %v, %v", n, err)
		return -1
	}
	Log.Debugf("Sent state = %v (0x%x), raw bytes = %x", state, state, bs)

	// For tests, we'll be more forgiving with acknowledgments
	if test.testMode {
		Log.Debugf("Test mode: not waiting for ACK")
		return 0
	}

	// Wait for acknowledgment with timeout
	ack := make([]byte, 4)
	test.ctrlConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err = io.ReadFull(test.ctrlConn, ack)
	test.ctrlConn.SetReadDeadline(time.Time{}) // Clear the deadline

	if err != nil {
		Log.Errorf("Failed to read state acknowledgment: %v", err)
		return -1
	}

	ackVal := binary.LittleEndian.Uint32(ack)
	Log.Debugf("Received %d bytes, raw = %x, interpreted as %x", n, ack, ackVal)

	if ackVal != ACK_SIGNAL {
		Log.Errorf("Received incorrect acknowledgment: expected %x, got %x", ACK_SIGNAL, ackVal)
		return -1
	}

	Log.Debugf("Received acknowledgment %x for state = %v", ACK_SIGNAL, state)
	return 0
}

func (test *IperfTest) newStream(conn net.Conn, senderFlag int) *iperfStream {
	sp := new(iperfStream)
	sp.role = senderFlag
	sp.conn = conn
	sp.test = test

	// mark, set sp.buffer
	sp.result = new(iperf_stream_results)
	sp.snd = test.proto.send
	sp.rcv = test.proto.recv
	sp.buffer = make([]byte, test.setting.blksize)

	copy(sp.buffer[:], "hello world!")

	// initialize stream
	// set tos bit. undo
	return sp
}

func (test *IperfTest) closeAllStreams() int {
	var err error

	for _, sp := range test.streams {
		err = sp.conn.Close()
		if err != nil {
			Log.Errorf("Stream close failed, err = %v", err)

			return -1
		}
	}

	return 0
}

func (test *IperfTest) checkThrottle(sp *iperfStream, now time.Time) {
	if sp.test.done {
		return
	}

	dur := now.Sub(sp.result.start_time)
	sec := dur.Seconds()

	bitsPerSecond := float64(sp.result.bytes_sent*8) / sec
	if bitsPerSecond < float64(sp.test.setting.rate) && sp.canSend == false {
		sp.canSend = true

		Log.Debugf("sp.can_send turn TRUE. bits_per_second = %6.2f MB/s Required = %6.2f MB/s",
			bitsPerSecond/MB_TO_B/8, float64(sp.test.setting.rate)/MB_TO_B/8)
	} else if bitsPerSecond > float64(sp.test.setting.rate) && sp.canSend == true {
		sp.canSend = false

		Log.Debugf("sp.can_send turn FALSE. bits_per_second = %6.2f MB/s Required = %6.2f MB/s",
			bitsPerSecond/MB_TO_B/8, float64(sp.test.setting.rate)/MB_TO_B/8)
	}
}

func (test *IperfTest) sendParams() int {
	Log.Debugf("Enter send_params")
	params := stream_params{
		ProtoName:     test.proto.name(),
		Reverse:       test.reverse,
		Duration:      test.duration,
		NoDelay:       test.noDelay,
		Interval:      test.interval,
		StreamNum:     test.streamNum,
		Blksize:       test.setting.blksize,
		SndWnd:        test.setting.sndWnd,
		RcvWnd:        test.setting.rcvWnd,
		ReadBufSize:   test.setting.readBufSize,
		WriteBufSize:  test.setting.writeBufSize,
		FlushInterval: test.setting.flushInterval,
		NoCong:        test.setting.noCong,
		FastResend:    test.setting.fastResend,
		DataShards:    test.setting.dataShards,
		ParityShards:  test.setting.parityShards,
		Burst:         test.setting.burst,
		Rate:          test.setting.rate,
		PacingTime:    test.setting.pacingTime,
	}
	bytes, err := json.Marshal(&params)
	if err != nil {
		Log.Errorf("Encode params failed: %v", err)
		return -1
	}
	Log.Debugf("Sending params: %s, bytes = %x", params.String(), bytes)

	// Prefix with length
	length := make([]byte, 4)
	binary.LittleEndian.PutUint32(length, uint32(len(bytes)))
	_, err = test.ctrlConn.Write(length)
	if err != nil {
		Log.Errorf("Write length failed: %v", err)
		return -1
	}

	n, err := test.ctrlConn.Write(bytes)
	if err != nil {
		Log.Errorf("Write failed: %v", err)
		return -1
	}
	Log.Debugf("Sent params %v bytes with length prefix", n)
	return 0
}

func (test *IperfTest) exchangeParams() int {
	if test.isServer == false {
		Log.Debugf("Client sending params")
		if test.sendParams() < 0 {
			Log.Errorf("Client sendParams failed")
			return -1
		}
		Log.Debugf("Client sent params successfully")
	} else {
		Log.Debugf("Server getting params")
		if test.getParams() < 0 {
			Log.Errorf("Server getParams failed")
			return -1
		}
		Log.Debugf("Server received params successfully")
	}
	return 0
}

func (test *IperfTest) getParams() int {
	Log.Debugf("Enter get_params")
	var params stream_params

	// Read length prefix
	lengthBuf := make([]byte, 4)
	_, err := io.ReadFull(test.ctrlConn, lengthBuf)
	if err != nil {
		Log.Errorf("Read length failed in getParams: %v", err)
		return -1
	}
	length := binary.LittleEndian.Uint32(lengthBuf)
	Log.Debugf("Received params length: %d", length)

	// Read exact JSON data
	buf := make([]byte, length)
	_, err = io.ReadFull(test.ctrlConn, buf)
	if err != nil {
		Log.Errorf("Read failed in getParams: %v", err)
		return -1
	}
	Log.Debugf("getParams read n = %d, bytes = %x, string = %s", len(buf), buf, buf)

	err = json.Unmarshal(buf, &params)
	if err != nil {
		Log.Errorf("Decode failed: %v", err)
		return -1
	}
	Log.Debugf("get params: %v", params.String())

	test.setProtocol(params.ProtoName)
	test.setTestReverse(params.Reverse)
	test.duration = params.Duration
	test.noDelay = params.NoDelay
	test.interval = params.Interval
	test.streamNum = params.StreamNum
	test.setting.blksize = params.Blksize
	test.setting.burst = params.Burst
	test.setting.rate = params.Rate
	test.setting.pacingTime = params.PacingTime
	test.setting.sndWnd = params.SndWnd
	test.setting.rcvWnd = params.RcvWnd
	test.setting.writeBufSize = params.WriteBufSize
	test.setting.readBufSize = params.ReadBufSize
	test.setting.flushInterval = params.FlushInterval
	test.setting.noCong = params.NoCong
	test.setting.fastResend = params.FastResend
	test.setting.dataShards = params.DataShards
	test.setting.parityShards = params.ParityShards
	return 0
}

func (test *IperfTest) sendResults() int {
	Log.Debugf("Send Results")

	var results = make(stream_results_array, test.streamNum)
	for i, sp := range test.streams {
		var bytesTransfer uint64
		if test.mode == IPERF_RECEIVER {
			bytesTransfer = sp.result.bytes_received
		} else {
			bytesTransfer = sp.result.bytes_sent
		}

		rp := sp.result
		spResult := stream_results_exchange{
			Id:        uint(i),
			Bytes:     bytesTransfer,
			Retrans:   rp.stream_retrans,
			Jitter:    0,
			InPkts:    rp.stream_in_pkts,
			OutPkts:   rp.stream_out_pkts,
			InSegs:    rp.stream_in_segs,
			OutSegs:   rp.stream_out_segs,
			Recovered: rp.stream_recovers,
			StartTime: sp.result.start_time,
			EndTime:   sp.result.end_time,
		}

		results[i] = spResult
	}

	bytes, err := json.Marshal(&results)
	if err != nil {
		Log.Error("Encode results failed. %v", err)

		return -1
	}

	// Prefix with length
	length := make([]byte, 4)
	binary.LittleEndian.PutUint32(length, uint32(len(bytes)))

	_, err = test.ctrlConn.Write(length)
	if err != nil {
		Log.Error("Write length failed. %v", err)

		return -1
	}

	n, err := test.ctrlConn.Write(bytes)
	if err != nil {
		Log.Error("Write failed. %v", err)

		return -1
	}

	Log.Debugf("Sent %d bytes of results", n)

	return 0
}

func (test *IperfTest) getResults() int {
	Log.Debugf("Enter get_results")

	var results = make(stream_results_array, test.streamNum)

	// Read length prefix
	lengthBuf := make([]byte, 4)

	_, err := io.ReadFull(test.ctrlConn, lengthBuf)
	if err != nil {
		Log.Errorf("Read length failed. %v", err)

		return -1
	}

	length := binary.LittleEndian.Uint32(lengthBuf)
	buf := make([]byte, length)

	_, err = io.ReadFull(test.ctrlConn, buf)
	if err != nil {
		Log.Errorf("Read failed. %v", err)

		return -1
	}

	err = json.Unmarshal(buf, &results)
	if err != nil {
		Log.Errorf("Decode failed. %v", err)

		return -1
	}

	Log.Debugf("Received %d bytes of results", len(buf))

	for i, result := range results {
		sp := test.streams[i]
		if test.mode == IPERF_RECEIVER {
			sp.result.bytes_sent = result.Bytes
			sp.result.stream_retrans = result.Retrans
			sp.result.stream_out_segs = result.OutSegs
			sp.result.stream_out_pkts = result.OutPkts
		} else {
			sp.result.bytes_received = result.Bytes
			sp.result.stream_in_segs = result.InSegs
			sp.result.stream_in_pkts = result.InPkts
			sp.result.stream_recovers = result.Recovered
		}
	}

	return 0
}

func (test *IperfTest) exchangeResults() int {
	if test.isServer == false {
		if test.sendResults() < 0 {
			return -1
		}
		if test.getResults() < 0 {
			return -1
		}
	} else {
		// server
		if test.getResults() < 0 {
			return -1
		}
		if test.sendResults() < 0 {
			return -1
		}
	}

	return 0
}

func (test *IperfTest) initTest() int {
	test.proto.init(test)

	now := time.Now()

	for _, sp := range test.streams {
		sp.result.start_time = now
		sp.result.start_time_fixed = now
	}

	return 0
}

func (test *IperfTest) Init() {
	test.protocols = append(test.protocols, new(TCPProto), new(rudpProto), new(kcpProto))
	test.testMode = false
}

func (test *IperfTest) ParseArguments() int {

	// command flag definition
	var helpFlag = flag.Bool("h", false, "this help")
	var serverFlag = flag.Bool("s", false, "server side")
	var clientFlag = flag.String("c", "127.0.0.1", "client side")
	var reverseFlag = flag.Bool("R", false, "reverse mode. client receive, server send")
	var portFlag = flag.Uint("p", 5201, "connect/listen port")
	var protocolFlag = flag.String("proto", TCP_NAME, "protocol under test")
	var durFlag = flag.Uint("d", 10, "duration (s)")
	var intervalFlag = flag.Uint("i", 1000, "test interval (ms)")
	var parallelFlag = flag.Uint("P", 1, "The number of simultaneous connections")
	var blksizeFlag = flag.Uint("l", 4*1024, "send/read block size")
	var bandwidthFlag = flag.String("b", "0", "bandwidth limit. (M/K), default MB/s")
	var debugFlag = flag.Bool("debug", false, "debug mode")
	var infoFlag = flag.Bool("info", false, "info mode")
	var noDelayFlag = flag.Bool("D", false, "no delay option")

	// RUDP specific option
	var sndWndFlag = flag.Uint("sw", 10, "rudp send window size")
	var rcvWndFlag = flag.Uint("rw", 512, "rudp receive window size")
	var readBufferSizeFlag = flag.Uint("rb", 4*1024, "read buffer size (Kb)")
	var writeBufferSizeFlag = flag.Uint("wb", 4*1024, "write buffer size (Kb)")
	var flushIntervalFlag = flag.Uint("f", 10, "flush interval for rudp (ms)")
	var noCongFlag = flag.Bool("nc", true, "no congestion control or BBR")
	var fastResendFlag = flag.Uint("fr", 0, "rudp fast resend strategy. 0 indicate turn off fast resend")
	var datashardsFlag = flag.Uint("data", 0, "rudp/kcp FEC dataShards option")
	var parityshardsFlag = flag.Uint("parity", 0, "rudp/kcp FEC parityShards option")
	// parse argument
	flag.Parse()

	if *helpFlag {
		flag.Usage()
		os.Exit(0)
	}

	// check valid
	flagset := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { flagset[f.Name] = true })

	if flagset["c"] == false {
		if *serverFlag == false {
			return -1
		}
	}

	validProtocol := false
	for _, proto := range ProtocolList {
		if *protocolFlag == proto {
			validProtocol = true
		}
	}

	if validProtocol == false {
		return -2
	}

	// set block size
	if flagset["l"] == false {
		if *protocolFlag == TCP_NAME {
			test.setting.blksize = DEFAULT_TCP_BLKSIZE
		} else if *protocolFlag == UDP_NAME {
			test.setting.blksize = DEFAULT_UDP_BLKSIZE
		} else if *protocolFlag == RUDP_NAME {
			test.setting.blksize = DEFAULT_RUDP_BLKSIZE
		} else if *protocolFlag == KCP_NAME {
			test.setting.blksize = DEFAULT_RUDP_BLKSIZE
		}
	} else {
		test.setting.blksize = *blksizeFlag
	}

	if flagset["b"] == false {
		test.setting.burst = true
	} else {
		test.setting.burst = false
		bwStr := *bandwidthFlag

		if string(bwStr[len(bwStr)-1]) == "M" {
			if n, err := strconv.Atoi(string(bwStr[:len(bwStr)-1])); err == nil {
				test.setting.rate = uint(n * MB_TO_B * 8)
			} else {
				Log.Errorf("Error bandwidth flag")
			}
		} else if string(bwStr[len(bwStr)-1]) == "K" {
			if n, err := strconv.Atoi(string(bwStr[:len(bwStr)-1])); err == nil {
				test.setting.rate = uint(n * KB_TO_B * 8)
			} else {
				Log.Errorf("Error bandwidth flag")
			}
		} else {
			if n, err := strconv.Atoi(bwStr); err == nil {
				test.setting.rate = uint(n * MB_TO_B * 8)
			} else {
				Log.Errorf("Error bandwidth flag")
			}
		}
		test.setting.pacingTime = 5 // 5ms pacing
	}

	if *debugFlag == true {
		SetLogLevelSafe(logging.DEBUG, "iperf")
		SetLogLevelSafe(logging.DEBUG, "rudp")
	} else if *infoFlag == true {
		SetLogLevelSafe(logging.INFO, "iperf")
		SetLogLevelSafe(logging.INFO, "rudp")
	} else {
		SetLogLevelSafe(logging.ERROR, "iperf")
		SetLogLevelSafe(logging.ERROR, "rudp")
	}

	// pass to iperf_test
	if *serverFlag == true {
		test.isServer = true
	} else {
		test.isServer = false

		var err error
		_, err = net.ResolveIPAddr("ip", *clientFlag)

		if err != nil {
			return -3
		}
		test.addr = *clientFlag
	}

	test.setTestReverse(*reverseFlag)
	test.port = *portFlag
	test.state = 0
	test.interval = *intervalFlag
	test.duration = *durFlag // 10s
	test.streamNum = *parallelFlag

	// rudp only
	test.setting.sndWnd = *sndWndFlag
	test.setting.rcvWnd = *rcvWndFlag
	test.setting.readBufSize = *readBufferSizeFlag * 1024 // Kb to b
	test.setting.writeBufSize = *writeBufferSizeFlag * 1024
	test.setting.flushInterval = *flushIntervalFlag
	test.setting.noCong = *noCongFlag
	test.setting.fastResend = *fastResendFlag
	test.setting.dataShards = *datashardsFlag
	test.setting.parityShards = *parityshardsFlag

	if test.interval > test.duration*1000 {
		Log.Errorf("interval must smaller than duration")
	}

	test.noDelay = *noDelayFlag
	if test.isServer == false {
		test.setProtocol(*protocolFlag)
	}

	test.Print()

	return 0
}

func (test *IperfTest) RunTest(wg *sync.WaitGroup) int {
	// server
	if test.isServer == true {
		rtn := test.runServer(wg)
		if rtn < 0 {
			Log.Errorf("Run server failed. %v", rtn)

			return rtn
		}
	} else {
		//client
		rtn := test.runClient()
		if rtn < 0 {
			Log.Errorf("Run client failed. %v", rtn)

			return rtn
		}
	}

	return 0
}

func (test *IperfTest) setTestReverse(reverse bool) {
	test.reverse = reverse
	if reverse == true {
		if test.isServer {
			test.mode = IPERF_SENDER
		} else {
			test.mode = IPERF_RECEIVER
		}
	} else {
		if test.isServer {
			test.mode = IPERF_RECEIVER
		} else {
			test.mode = IPERF_SENDER
		}
	}
}

func (test *IperfTest) FreeTest() int {
	return 0
}

func (test *IperfTest) Print() {
	if test.isServer {
		return
	}
	if test.proto == nil {
		Log.Errorf("Protocol not set.")

		return
	}

	fmt.Printf("Iperf started:\n")
	if test.proto.name() == TCP_NAME {
		fmt.Printf("addr:%v\tport:%v\tproto:%v\tinterval:%v\tduration:%v\tNoDelay:%v\tburst:%v\tBlockSize:%v\tStreamNum:%v\n",
			test.addr, test.port, test.proto.name(), test.interval, test.duration, test.noDelay, test.setting.burst, test.setting.blksize, test.streamNum)
	} else if test.proto.name() == RUDP_NAME {
		fmt.Printf("addr:%v\tport:%v\tproto:%v\tinterval:%v\tduration:%v\tNoDelay:%v\tburst:%v\tBlockSize:%v\tStreamNum:%v\tfr:%v\n"+
			"RUDP settting: sndWnd:%v\trcvWnd:%v\twriteBufSize:%vKb\treadBufSize:%vKb\tnoCongestion:%v\tflushInterval:%v\tdataShards:%v\tparityShards:%v\n",
			test.addr, test.port, test.proto.name(), test.interval, test.duration, test.noDelay, test.setting.burst, test.setting.blksize, test.streamNum, test.setting.fastResend,
			test.setting.sndWnd, test.setting.rcvWnd, test.setting.writeBufSize/1024, test.setting.readBufSize/1024, test.setting.noCong,
			test.setting.flushInterval, test.setting.dataShards, test.setting.parityShards)
	} else if test.proto.name() == KCP_NAME {
		fmt.Printf("addr:%v\tport:%v\tproto:%v\tinterval:%v\tduration:%v\tNoDelay:%v\tburst:%v\tBlockSize:%v\tStreamNum:%v\n"+
			"KCP settting: sndWnd:%v\trcvWnd:%v\twriteBufSize:%vKb\treadBufSize:%vKb\tnoCongestion:%v\tflushInterval:%v\tdataShards:%v\tparityShards:%v\n",
			test.addr, test.port, test.proto.name(), test.interval, test.duration, test.noDelay, test.setting.burst, test.setting.blksize, test.streamNum,
			test.setting.sndWnd, test.setting.rcvWnd, test.setting.writeBufSize/1024, test.setting.readBufSize/1024, test.setting.noCong,
			test.setting.flushInterval, test.setting.dataShards, test.setting.parityShards)
	}
}

func (sp *iperfStream) iperfRecv(test *IperfTest) {
	// travel all the stream and start receive
	for {
		var n int
		if n = sp.rcv(sp); n < 0 {
			if n == -1 {
				Log.Debugf("Stream Quit receiving")
				return
			}

			Log.Errorf("Iperf streams receive failed. n = %v", n)
			return
		}

		sp.mu.Lock() // Lock before updating shared fields
		if test.state == TEST_RUNNING {
			test.mu.Lock()
			test.bytesReceived += uint64(n)
			test.blocksReceived += 1
			test.mu.Unlock()

			Log.Debugf("Stream receive data %v bytes of total %v bytes", n, test.bytesReceived)
		}
		sp.mu.Unlock() // Unlock after updates

		test.mu.Lock()
		done := test.done
		test.mu.Unlock()

		if done {
			test.ctrlChan <- TEST_END
			Log.Debugf("Stream quit receiving. test done.")
			return
		}
	}
}

// iperfSend -- called by multi streams
func (sp *iperfStream) iperfSend(test *IperfTest) {
	// defaultRate := uint64(1000 * 1000 * 1000) // 1 Gb/s in bits (1000 Mbps)
	sendInterval := time.Duration(1000000) // 1 ms for exactly 1000 sends/s
	if !test.setting.burst && test.setting.rate != 0 {
		sendInterval = time.Duration(uint64(sp.bufferSize()) * 8 * 1000000000 / uint64(test.setting.rate)) // ns
	}

	Log.Debugf("Send interval set to %v", sendInterval)

	ticker := time.NewTicker(sendInterval)
	defer ticker.Stop()

	for {
		test.mu.Lock()
		if test.done {
			test.mu.Unlock()
			test.ctrlChan <- TEST_END
			return
		}
		test.mu.Unlock()

		select {
		case <-ticker.C:
			if sp.canSend {
				n := sp.snd(sp)
				if n < 0 {
					return
				}
				test.mu.Lock()
				test.bytesSent += uint64(n)
				test.blocksSent++
				test.mu.Unlock()
			}
		}

		if test.setting.burst == false {
			test.checkThrottle(sp, time.Now())
		}

		if (test.duration != 0 && test.done) ||
			(test.setting.bytes != 0 && test.bytesSent >= test.setting.bytes) ||
			(test.setting.blocks != 0 && test.blocksSent >= test.setting.blocks) {

			test.ctrlChan <- TEST_END

			Log.Debugf("Stream Quit sending")

			return
		}
	}
}

func (sp *iperfStream) bufferSize() int {
	return len(sp.buffer)
}

func (test *IperfTest) createSenderTicker() int {
	for _, sp := range test.streams {
		sp.canSend = true

		if test.setting.rate != 0 {
			if test.setting.pacingTime == 0 || test.setting.burst == true {
				Log.Error("pacing_time & rate & burst should be set at the same time.")

				return -1
			}

			var cd TimerClientData

			cd.p = sp
			sp.sendTicker = tickerCreate(time.Now(), sendTickerProc, cd, test.setting.pacingTime, ^uint(0))
		}
	}

	return 0
}

// iperfReporterCallback is called by the IperfTest instance when a report needs to be printed.
func iperfReporterCallback(test *IperfTest) {
	<-test.chStats // only call this function after stats
	if test.state == TEST_RUNNING {
		Log.Debugf("TEST_RUNNING report, role = %v, mode = %v, done = %v", test.isServer, test.mode, test.done)

		test.iperfPrintIntermediate()
	} else if test.state == TEST_END || test.state == IPERF_DISPLAY_RESULT {
		Log.Debugf("TEST_END report, role = %v, mode = %v, done = %v", test.isServer, test.mode, test.done)

		test.iperfPrintIntermediate()
		test.iperfPrintResults()
	} else {
		Log.Errorf("Unexpected state = %v, role = %v", test.state, test.isServer)
	}
}

func (test *IperfTest) iperfPrintIntermediate() {
	var sumBytesTransfer, sumRtt uint64
	var sumRetrans uint
	var displayStartTime, displayEndTime float64

	for i, sp := range test.streams {
		if i == 0 && len(sp.result.interval_results) == 1 {
			// first time to print result, print header
			if test.proto.name() == TCP_NAME {
				fmt.Printf(TCP_INTERVAL_HEADER)
			} else {
				fmt.Printf(RUDP_INTERVAL_HEADER)
			}
		}

		intervalSeq := len(sp.result.interval_results) - 1
		rp := sp.result.interval_results[intervalSeq] // get the last one

		supposedStartTime := time.Duration(uint(intervalSeq)*test.interval) * time.Millisecond
		realStartTime := rp.interval_start_time.Sub(sp.result.start_time)
		realEndTime := rp.interval_end_time.Sub(sp.result.start_time)

		if durNotSame(supposedStartTime, realStartTime) {
			Log.Errorf("Start time differ from expected. supposed = %v, real = %v",
				supposedStartTime.Nanoseconds()/MS_TO_NS, realStartTime.Nanoseconds()/MS_TO_NS)
			//return
		}

		sumBytesTransfer += rp.bytes_transfered
		sumRetrans += rp.interval_retrans
		sumRtt += uint64(rp.rtt)

		displayStartTime = float64(realStartTime.Nanoseconds()) / S_TO_NS
		displayEndTime = float64(realEndTime.Nanoseconds()) / S_TO_NS

		displayBytesTransfer := float64(rp.bytes_transfered) / MB_TO_B
		displayBandwidth := displayBytesTransfer / float64(test.interval) * 1000 * 8 // Mb/s

		// output single stream interval report
		if test.proto.name() == TCP_NAME {
			//display_retrans_rate :=  float64(rp.interval_retrans) / (float64(rp.bytes_transfered) / TCP_MSS) * 100
			fmt.Printf(TCP_REPORT_SINGLE_STREAM, i, displayStartTime, displayEndTime,
				displayBytesTransfer, displayBandwidth, float64(rp.rtt)/1000, rp.interval_retrans)
		} else {
			totalSegs := float64(rp.bytes_transfered)/RUDP_MSS + float64(rp.interval_retrans)

			displayRetransRate := float64(rp.interval_retrans) / totalSegs * 100 // to percentage
			displayLostRate := float64(rp.interval_lost) / totalSegs * 100
			displayEarlyRetransRate := float64(rp.interval_early_retrans) / totalSegs * 100
			displayFastRetransRate := float64(rp.interval_fast_retrans) / totalSegs * 100

			fmt.Printf(RUDP_REPORT_SINGLE_STREAM, i, displayStartTime, displayEndTime, displayBytesTransfer,
				displayBandwidth, float64(rp.rtt)/1000, rp.interval_retrans, displayRetransRate,
				displayLostRate, displayEarlyRetransRate, displayFastRetransRate)
		}
	}

	if test.streamNum > 1 {
		displaySumBytesTransfer := float64(sumBytesTransfer) / MB_TO_B
		displayBandwidth := displaySumBytesTransfer / float64(test.interval) * 1000 * 8

		fmt.Printf(REPORT_SUM_STREAM, displayStartTime, displayEndTime, displaySumBytesTransfer,
			displayBandwidth, float64(sumRtt)/1000/float64(test.streamNum), sumRetrans)

		fmt.Printf(REPORT_SEPERATOR)
	}
}

func durNotSame(d time.Duration, d2 time.Duration) bool {
	// if deviation exceed 1ms, there might be problems
	var diffInMs int = int(d.Nanoseconds()/MS_TO_NS - d2.Nanoseconds()/MS_TO_NS)
	if diffInMs < -100 || diffInMs > 100 {
		return true
	}

	return false
}

func (test *IperfTest) iperfPrintResults() {
	fmt.Printf(SUMMARY_SEPERATOR)
	if test.proto.name() == TCP_NAME {
		fmt.Printf(TCP_RESULT_HEADER)
	} else {
		fmt.Printf(RUDP_RESULT_HEADER)
	}

	if len(test.streams) <= 0 {
		Log.Errorf("No streams available.")

		return
	}

	var sumBytesTransfer uint64
	var sumRetrans uint
	var avgRtt float64
	var displayStartTime, displayEndTime float64

	for i, sp := range test.streams {
		displayStartTime = float64(0)
		displayEndTime = float64(sp.result.end_time.Sub(sp.result.start_time).Nanoseconds()) / S_TO_NS

		var displayBytesTransfer float64

		if test.mode == IPERF_RECEIVER {
			displayBytesTransfer = float64(sp.result.bytes_received) / MB_TO_B
			sumBytesTransfer += sp.result.bytes_received
		} else {
			displayBytesTransfer = float64(sp.result.bytes_sent) / MB_TO_B
			sumBytesTransfer += sp.result.bytes_sent
		}

		displayRtt := float64(sp.result.stream_sum_rtt) / float64(sp.result.stream_cnt_rtt) / 1000
		avgRtt += displayRtt
		displayBandwidth := displayBytesTransfer / float64(test.duration) * 8 // Mb/s

		sumRetrans += sp.result.stream_retrans

		var role string

		if sp.role == SENDER_STREAM {
			role = "SENDER"
		} else {
			role = "RECEIVER"
		}

		// output single stream final report
		if test.proto.name() == TCP_NAME {
			totalSegs := (displayBytesTransfer * MB_TO_B / TCP_MSS) + float64(sp.result.stream_retrans)
			displayRetransRate := float64(sp.result.stream_retrans) / totalSegs * 100
			fmt.Printf(TCP_REPORT_SINGLE_RESULT, i, displayStartTime, displayEndTime, displayBytesTransfer,
				displayBandwidth, displayRtt, sp.result.stream_retrans, displayRetransRate, role)
		} else {
			totalSegs := float64(sp.result.stream_out_segs)

			displayRetransRate := float64(sp.result.stream_retrans) / totalSegs * 100
			displayLostRate := float64(sp.result.stream_lost) / totalSegs * 100
			displayEarlyRetransRate := float64(sp.result.stream_early_retrans) / totalSegs * 100
			displayFastRetransRate := float64(sp.result.stream_fast_retrans) / totalSegs * 100

			recoverRate := float64(sp.result.stream_recovers) / totalSegs * 100
			pktsLostRate := (1 - float64(sp.result.stream_in_pkts)/float64(sp.result.stream_out_pkts)) * 100
			segsLostRate := (1 - float64(sp.result.stream_in_segs)/float64(sp.result.stream_out_segs)) * 100

			fmt.Printf(RUDP_REPORT_SINGLE_RESULT, i, displayStartTime, displayEndTime, displayBytesTransfer,
				displayBandwidth, displayRtt, sp.result.stream_retrans, displayRetransRate,
				displayLostRate, displayEarlyRetransRate, displayFastRetransRate,
				recoverRate, pktsLostRate, segsLostRate, role)
			fmt.Printf("total_segs = %v, out_segs = %v, in_segs = %v, out_pkts = %v, in_pkts = %v, recovery = %v\n, repeat = %v\n",
				totalSegs, sp.result.stream_out_segs, sp.result.stream_in_segs, sp.result.stream_out_pkts, sp.result.stream_in_pkts, sp.result.stream_recovers, sp.result.stream_repeat_segs)
		}
	}

	if test.streamNum > 1 {
		displaySumBytesTransfer := float64(sumBytesTransfer) / MB_TO_B
		displayBandwidth := displaySumBytesTransfer / float64(test.duration) * 1000 * 8

		fmt.Printf(REPORT_SUM_STREAM, displayStartTime, displayEndTime,
			displaySumBytesTransfer, displayBandwidth, avgRtt/float64(test.streamNum), sumRetrans)
	}
}

// Gather statistics during a test.
func iperfStatsCallback(test *IperfTest) {
	test.mu.Lock()

	if test.done {
		test.mu.Unlock()
		test.chStats <- true
		return
	}

	test.mu.Unlock()

	for _, sp := range test.streams {
		sp.mu.Lock() // Lock each stream during stats collection

		tempResult := iperf_interval_results{}
		rp := sp.result

		if len(rp.interval_results) == 0 {
			// first interval
			tempResult.interval_start_time = rp.start_time
		} else {
			tempResult.interval_start_time = rp.end_time // rp.end_time contains timestamp of previous interval
		}

		rp.end_time = time.Now()

		tempResult.interval_end_time = rp.end_time
		tempResult.interval_dur = tempResult.interval_end_time.Sub(tempResult.interval_start_time)

		test.proto.statsCallback(test, sp, &tempResult) // write temp_result differ from proto to proto
		if test.mode == IPERF_RECEIVER {
			tempResult.bytes_transfered = rp.bytes_received_this_interval
		} else {
			tempResult.bytes_transfered = rp.bytes_sent_this_interval
		}

		rp.interval_results = append(rp.interval_results, tempResult)
		rp.bytes_sent_this_interval = 0
		rp.bytes_received_this_interval = 0

		sp.mu.Unlock() // Unlock after stats collection
	}
	test.chStats <- true
}
