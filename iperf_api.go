package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/op/go-logging"
)

func newIperfTest() (test *iperf_test) {
	test = new(iperf_test)
	test.ctrl_chan = make(chan uint, 5)
	test.setting = new(iperf_setting)
	test.reporter_callback = iperfReporterCallback
	test.stats_callback = iperfStatsCallback
	test.chStats = make(chan bool, 1)
	return
}

func (test *iperf_test) setProtocol(proto_name string) int {
	for _, proto := range test.protocols {
		if proto_name == proto.name() {
			test.proto = proto
			return 0
		}
	}
	return -1
}

func (test *iperf_test) setSendState(state uint) int {
	test.state = state
	test.ctrl_chan <- test.state
	bs := make([]byte, 4)
	binary.LittleEndian.PutUint32(bs, uint32(state))
	//msg := fmt.Sprintf("%v", test.state)
	n, err := test.ctrl_conn.Write(bs)
	if err != nil {
		log.Errorf("Write state error. %v %v", n, err)
		return -1
	}
	log.Debugf("Set & send state = %v, n = %v", state, n)
	return 0
}

func (test *iperf_test) newStream(conn net.Conn, sender_flag int) *iperf_stream {
	sp := new(iperf_stream)
	sp.role = sender_flag
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

func (test *iperf_test) closeAllStreams() int {
	var err error
	for _, sp := range test.streams {
		err = sp.conn.Close()
		if err != nil {
			log.Errorf("Stream close failed, err = %v", err)
			return -1
		}
	}
	return 0
}

func (test *iperf_test) checkThrottle(sp *iperf_stream, now time.Time) {
	if sp.test.done {
		return
	}

	dur := now.Sub(sp.result.start_time)
	sec := dur.Seconds()

	bitsPerSecond := float64(sp.result.bytes_sent*8) / sec
	if bitsPerSecond < float64(sp.test.setting.rate) && sp.can_send == false {
		sp.can_send = true

		log.Debugf("sp.can_send turn TRUE. bits_per_second = %6.2f MB/s Required = %6.2f MB/s",
			bitsPerSecond/MB_TO_B/8, float64(sp.test.setting.rate)/MB_TO_B/8)
	} else if bitsPerSecond > float64(sp.test.setting.rate) && sp.can_send == true {
		sp.can_send = false

		log.Debugf("sp.can_send turn FALSE. bits_per_second = %6.2f MB/s Required = %6.2f MB/s",
			bitsPerSecond/MB_TO_B/8, float64(sp.test.setting.rate)/MB_TO_B/8)
	}
}

func (test *iperf_test) sendParams() int {
	log.Debugf("Enter send_params")
	params := stream_params{
		ProtoName:     test.proto.name(),
		Reverse:       test.reverse,
		Duration:      test.duration,
		NoDelay:       test.no_delay,
		Interval:      test.interval,
		StreamNum:     test.stream_num,
		Blksize:       test.setting.blksize,
		SndWnd:        test.setting.snd_wnd,
		RcvWnd:        test.setting.rcv_wnd,
		ReadBufSize:   test.setting.read_buf_size,
		WriteBufSize:  test.setting.write_buf_size,
		FlushInterval: test.setting.flush_interval,
		NoCong:        test.setting.no_cong,
		FastResend:    test.setting.fast_resend,
		DataShards:    test.setting.data_shards,
		ParityShards:  test.setting.parity_shards,
		Burst:         test.setting.burst,
		Rate:          test.setting.rate,
		PacingTime:    test.setting.pacing_time,
	}

	bytes, err := json.Marshal(&params)
	if err != nil {
		log.Error("Encode params failed. %v", err)

		return -1
	}

	n, err := test.ctrl_conn.Write(bytes)
	if err != nil {
		log.Error("Write failed. %v", err)

		return -1
	}

	log.Debugf("send params %v bytes: %v", n, params.String())

	return 0
}

func (test *iperf_test) getParams() int {
	log.Debugf("Enter get_params")
	var params stream_params

	buf := make([]byte, 1024)

	n, err := test.ctrl_conn.Read(buf)
	if err != nil {
		log.Errorf("Read failed. %v", err)

		return -1
	}

	err = json.Unmarshal(buf[:n], &params)
	if err != nil {
		log.Errorf("Decode failed. %v", err)

		return -1
	}

	log.Debugf("get params %v bytes: %v", n, params.String())

	test.setProtocol(params.ProtoName)
	test.setTestReverse(params.Reverse)
	test.duration = params.Duration
	test.no_delay = params.NoDelay
	test.interval = params.Interval
	test.stream_num = params.StreamNum
	test.setting.blksize = params.Blksize
	test.setting.burst = params.Burst
	test.setting.rate = params.Rate
	test.setting.pacing_time = params.PacingTime

	// rudp/kcp only
	test.setting.snd_wnd = params.SndWnd
	test.setting.rcv_wnd = params.RcvWnd
	test.setting.write_buf_size = params.WriteBufSize
	test.setting.read_buf_size = params.ReadBufSize
	test.setting.flush_interval = params.FlushInterval
	test.setting.no_cong = params.NoCong
	test.setting.fast_resend = params.FastResend
	test.setting.data_shards = params.DataShards
	test.setting.parity_shards = params.ParityShards
	return 0
}

func (test *iperf_test) exchangeParams() int {
	if test.is_server == false {
		if test.sendParams() < 0 {
			return -1
		}
	} else {
		if test.getParams() < 0 {
			return -1
		}
	}

	return 0
}

func (test *iperf_test) sendResults() int {
	log.Debugf("Send Results")

	var results = make(stream_results_array, test.stream_num)
	for i, sp := range test.streams {
		var bytes_transfer uint64
		if test.mode == IPERF_RECEIVER {
			bytes_transfer = sp.result.bytes_received
		} else {
			bytes_transfer = sp.result.bytes_sent
		}

		rp := sp.result
		sp_result := stream_results_exchange{
			Id:        uint(i),
			Bytes:     bytes_transfer,
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

		results[i] = sp_result
	}

	bytes, err := json.Marshal(&results)
	if err != nil {
		log.Error("Encode results failed. %v", err)

		return -1
	}

	// Prefix with length
	length := make([]byte, 4)
	binary.LittleEndian.PutUint32(length, uint32(len(bytes)))

	_, err = test.ctrl_conn.Write(length)
	if err != nil {
		log.Error("Write length failed. %v", err)

		return -1
	}

	n, err := test.ctrl_conn.Write(bytes)
	if err != nil {
		log.Error("Write failed. %v", err)

		return -1
	}

	log.Debugf("Sent %d bytes of results", n)

	return 0
}

func (test *iperf_test) getResults() int {
	log.Debugf("Enter get_results")

	var results = make(stream_results_array, test.stream_num)

	// Read length prefix
	lengthBuf := make([]byte, 4)

	_, err := io.ReadFull(test.ctrl_conn, lengthBuf)
	if err != nil {
		log.Errorf("Read length failed. %v", err)

		return -1
	}

	length := binary.LittleEndian.Uint32(lengthBuf)
	buf := make([]byte, length)

	_, err = io.ReadFull(test.ctrl_conn, buf)
	if err != nil {
		log.Errorf("Read failed. %v", err)

		return -1
	}

	err = json.Unmarshal(buf, &results)
	if err != nil {
		log.Errorf("Decode failed. %v", err)

		return -1
	}

	log.Debugf("Received %d bytes of results", len(buf))

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

func (test *iperf_test) exchangeResults() int {
	if test.is_server == false {
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

func (test *iperf_test) initTest() int {
	test.proto.init(test)

	now := time.Now()

	for _, sp := range test.streams {
		sp.result.start_time = now
		sp.result.start_time_fixed = now
	}

	return 0
}

func (test *iperf_test) init() {
	test.protocols = append(test.protocols, new(TCPProto), new(rudp_proto), new(kcp_proto))
}

func (test *iperf_test) parseArguments() int {

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
	for _, proto := range PROTOCOL_LIST {
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
		bw_str := *bandwidthFlag

		if string(bw_str[len(bw_str)-1]) == "M" {
			if n, err := strconv.Atoi(string(bw_str[:len(bw_str)-1])); err == nil {
				test.setting.rate = uint(n * MB_TO_B * 8)
			} else {
				log.Errorf("Error bandwidth flag")
			}
		} else if string(bw_str[len(bw_str)-1]) == "K" {
			if n, err := strconv.Atoi(string(bw_str[:len(bw_str)-1])); err == nil {
				test.setting.rate = uint(n * KB_TO_B * 8)
			} else {
				log.Errorf("Error bandwidth flag")
			}
		} else {
			if n, err := strconv.Atoi(bw_str); err == nil {
				test.setting.rate = uint(n * MB_TO_B * 8)
			} else {
				log.Errorf("Error bandwidth flag")
			}
		}
		test.setting.pacing_time = 5 // 5ms pacing
	}

	if *debugFlag == true {
		logging.SetLevel(logging.DEBUG, "iperf")
		logging.SetLevel(logging.DEBUG, "rudp")
	} else if *infoFlag == true {
		logging.SetLevel(logging.INFO, "iperf")
		logging.SetLevel(logging.INFO, "rudp")
	} else {
		logging.SetLevel(logging.ERROR, "iperf")
		logging.SetLevel(logging.ERROR, "rudp")
	}

	// pass to iperf_test
	if *serverFlag == true {
		test.is_server = true
	} else {
		test.is_server = false

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
	test.stream_num = *parallelFlag

	// rudp only
	test.setting.snd_wnd = *sndWndFlag
	test.setting.rcv_wnd = *rcvWndFlag
	test.setting.read_buf_size = *readBufferSizeFlag * 1024 // Kb to b
	test.setting.write_buf_size = *writeBufferSizeFlag * 1024
	test.setting.flush_interval = *flushIntervalFlag
	test.setting.no_cong = *noCongFlag
	test.setting.fast_resend = *fastResendFlag
	test.setting.data_shards = *datashardsFlag
	test.setting.parity_shards = *parityshardsFlag

	if test.interval > test.duration*1000 {
		log.Errorf("interval must smaller than duration")
	}

	test.no_delay = *noDelayFlag
	if test.is_server == false {
		test.setProtocol(*protocolFlag)
	}

	test.Print()

	return 0
}

func (test *iperf_test) runTest() int {
	// server
	if test.is_server == true {
		rtn := test.run_server()
		if rtn < 0 {
			log.Errorf("Run server failed. %v", rtn)

			return rtn
		}
	} else {
		//client
		rtn := test.runClient()
		if rtn < 0 {
			log.Errorf("Run client failed. %v", rtn)

			return rtn
		}
	}

	return 0
}

func (test *iperf_test) setTestReverse(reverse bool) {
	test.reverse = reverse
	if reverse == true {
		if test.is_server {
			test.mode = IPERF_SENDER
		} else {
			test.mode = IPERF_RECEIVER
		}
	} else {
		if test.is_server {
			test.mode = IPERF_RECEIVER
		} else {
			test.mode = IPERF_SENDER
		}
	}
}

func (test *iperf_test) freeTest() int {
	return 0
}

func (test *iperf_test) Print() {
	if test.is_server {
		return
	}
	if test.proto == nil {
		log.Errorf("Protocol not set.")

		return
	}

	fmt.Printf("Iperf started:\n")
	if test.proto.name() == TCP_NAME {
		fmt.Printf("addr:%v\tport:%v\tproto:%v\tinterval:%v\tduration:%v\tNoDelay:%v\tburst:%v\tBlockSize:%v\tStreamNum:%v\n",
			test.addr, test.port, test.proto.name(), test.interval, test.duration, test.no_delay, test.setting.burst, test.setting.blksize, test.stream_num)
	} else if test.proto.name() == RUDP_NAME {
		fmt.Printf("addr:%v\tport:%v\tproto:%v\tinterval:%v\tduration:%v\tNoDelay:%v\tburst:%v\tBlockSize:%v\tStreamNum:%v\tfr:%v\n"+
			"RUDP settting: sndWnd:%v\trcvWnd:%v\twriteBufSize:%vKb\treadBufSize:%vKb\tnoCongestion:%v\tflushInterval:%v\tdataShards:%v\tparityShards:%v\n",
			test.addr, test.port, test.proto.name(), test.interval, test.duration, test.no_delay, test.setting.burst, test.setting.blksize, test.stream_num, test.setting.fast_resend,
			test.setting.snd_wnd, test.setting.rcv_wnd, test.setting.write_buf_size/1024, test.setting.read_buf_size/1024, test.setting.no_cong,
			test.setting.flush_interval, test.setting.data_shards, test.setting.parity_shards)
	} else if test.proto.name() == KCP_NAME {
		fmt.Printf("addr:%v\tport:%v\tproto:%v\tinterval:%v\tduration:%v\tNoDelay:%v\tburst:%v\tBlockSize:%v\tStreamNum:%v\n"+
			"KCP settting: sndWnd:%v\trcvWnd:%v\twriteBufSize:%vKb\treadBufSize:%vKb\tnoCongestion:%v\tflushInterval:%v\tdataShards:%v\tparityShards:%v\n",
			test.addr, test.port, test.proto.name(), test.interval, test.duration, test.no_delay, test.setting.burst, test.setting.blksize, test.stream_num,
			test.setting.snd_wnd, test.setting.rcv_wnd, test.setting.write_buf_size/1024, test.setting.read_buf_size/1024, test.setting.no_cong,
			test.setting.flush_interval, test.setting.data_shards, test.setting.parity_shards)
	}
}

// iperf_stream

func (sp *iperf_stream) iperfRecv(test *iperf_test) {
	// travel all the stream and start receive
	for {
		var n int
		if n = sp.rcv(sp); n < 0 {
			if n == -1 {
				log.Debugf("Stream Quit receiving")

				return
			}

			log.Errorf("Iperf streams receive failed. n = %v", n)

			return
		}

		if test.state == TEST_RUNNING {
			test.bytes_received += uint64(n)
			test.blocks_received += 1

			log.Debugf("Stream receive data %v bytes of total %v bytes", n, test.bytes_received)
		}

		if test.done {
			test.ctrl_chan <- TEST_END
			log.Debugf("Stream quit receiving. test done.")

			return
		}
	}
}

// iperfSend -- called by multi streams
func (sp *iperf_stream) iperfSend(test *iperf_test) {
	// defaultRate := uint64(1000 * 1000 * 1000) // 1 Gb/s in bits (1000 Mbps)
	sendInterval := time.Duration(1000000) // 1 ms for exactly 1000 sends/s
	if !test.setting.burst && test.setting.rate != 0 {
		sendInterval = time.Duration(uint64(sp.bufferSize()) * 8 * 1000000000 / uint64(test.setting.rate)) // ns
	}

	log.Debugf("Send interval set to %v", sendInterval)

	ticker := time.NewTicker(sendInterval)
	defer ticker.Stop()

	for {
		select {
		case t := <-ticker.C:
			if sp.can_send {
				n := sp.snd(sp)
				if n < 0 {
					if n == -1 {
						log.Debugf("Iperf send stream closed.")
						return
					}

					log.Error("Iperf streams send failed. %v", n)

					return
				}

				test.bytes_sent += uint64(n)
				test.blocks_sent += 1

				log.Debugf("Stream sent data %v bytes at %v, total %v bytes", n, t, test.bytes_sent)
			}
		}

		if test.setting.burst == false {
			test.checkThrottle(sp, time.Now())
		}

		if (test.duration != 0 && test.done) ||
			(test.setting.bytes != 0 && test.bytes_sent >= test.setting.bytes) ||
			(test.setting.blocks != 0 && test.blocks_sent >= test.setting.blocks) {
			test.ctrl_chan <- TEST_END

			log.Debugf("Stream Quit sending")

			return
		}
	}
}

func (sp *iperf_stream) bufferSize() int {
	return len(sp.buffer)
}

func (test *iperf_test) createSenderTicker() int {
	for _, sp := range test.streams {
		sp.can_send = true

		if test.setting.rate != 0 {
			if test.setting.pacing_time == 0 || test.setting.burst == true {
				log.Error("pacing_time & rate & burst should be set at the same time.")

				return -1
			}

			var cd TimerClientData

			cd.p = sp
			sp.send_ticker = ticker_create(time.Now(), sendTickerProc, cd, test.setting.pacing_time, ^uint(0))
		}
	}

	return 0
}

// iperfReporterCallback is called by the iperf_test instance when a report needs to be printed.
func iperfReporterCallback(test *iperf_test) {
	<-test.chStats // only call this function after stats
	if test.state == TEST_RUNNING {
		log.Debugf("TEST_RUNNING report, role = %v, mode = %v, done = %v", test.is_server, test.mode, test.done)

		test.iperfPrintIntermediate()
	} else if test.state == TEST_END || test.state == IPERF_DISPLAY_RESULT {
		log.Debugf("TEST_END report, role = %v, mode = %v, done = %v", test.is_server, test.mode, test.done)

		test.iperfPrintIntermediate()
		test.iperfPrintResults()
	} else {
		log.Errorf("Unexpected state = %v, role = %v", test.state, test.is_server)
	}
}

func (test *iperf_test) iperfPrintIntermediate() {
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
			log.Errorf("Start time differ from expected. supposed = %v, real = %v",
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

	if test.stream_num > 1 {
		displaySumBytesTransfer := float64(sumBytesTransfer) / MB_TO_B
		displayBandwidth := displaySumBytesTransfer / float64(test.interval) * 1000 * 8

		fmt.Printf(REPORT_SUM_STREAM, displayStartTime, displayEndTime, displaySumBytesTransfer,
			displayBandwidth, float64(sumRtt)/1000/float64(test.stream_num), sumRetrans)

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

func (test *iperf_test) iperfPrintResults() {
	fmt.Printf(SUMMARY_SEPERATOR)
	if test.proto.name() == TCP_NAME {
		fmt.Printf(TCP_RESULT_HEADER)
	} else {
		fmt.Printf(RUDP_RESULT_HEADER)
	}

	if len(test.streams) <= 0 {
		log.Errorf("No streams available.")

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

	if test.stream_num > 1 {
		displaySumBytesTransfer := float64(sumBytesTransfer) / MB_TO_B
		displayBandwidth := displaySumBytesTransfer / float64(test.duration) * 1000 * 8

		fmt.Printf(REPORT_SUM_STREAM, displayStartTime, displayEndTime,
			displaySumBytesTransfer, displayBandwidth, avgRtt/float64(test.stream_num), sumRetrans)
	}
}

// Gather statistics during a test.
func iperfStatsCallback(test *iperf_test) {
	for _, sp := range test.streams {
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

		test.proto.stats_callback(test, sp, &tempResult) // write temp_result differ from proto to proto
		if test.mode == IPERF_RECEIVER {
			tempResult.bytes_transfered = rp.bytes_received_this_interval
		} else {
			tempResult.bytes_transfered = rp.bytes_sent_this_interval
		}

		rp.interval_results = append(rp.interval_results, tempResult)
		rp.bytes_sent_this_interval = 0
		rp.bytes_received_this_interval = 0
	}
	test.chStats <- true
}
