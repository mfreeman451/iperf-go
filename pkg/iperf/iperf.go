package iperf

import (
	"fmt"
	"net"
	"sync"
	"time"
)

var ProtocolList = []string{"tcp", "udp", "rudp", "kcp"}

const (
	IPERF_START           = 1
	IPERF_DONE            = 2
	IPERF_CREATE_STREAM   = 3
	IPERF_EXCHANGE_PARAMS = 4
	IPERF_EXCHANGE_RESULT = 5
	IPERF_DISPLAY_RESULT  = 6

	STREAM_CLOSE = 10

	SENDER_STREAM   = 20
	RECEIVER_STREAM = 21

	TEST_START          = 31
	TEST_RUNNING        = 32
	TEST_RESULT_REQUEST = 33
	TEST_END            = 34
	/* unexpected situation */
	CLIENT_TERMINATE = 50
	SERVER_TERMINATE = 51

	IPERF_SENDER   = true
	IPERF_RECEIVER = false
)

const (
	TCP_NAME  = "tcp"
	UDP_NAME  = "udp"
	RUDP_NAME = "rudp"
	KCP_NAME  = "kcp"
)

const (
	DEFAULT_TCP_BLKSIZE  = 128 * 1024 // default read/write block size
	DEFAULT_UDP_BLKSIZE  = 1460       // default is dynamically set
	DEFAULT_RUDP_BLKSIZE = 4 * 1024   // default read/write block size
	TCP_MSS              = 1460       // tcp mss size
	RUDP_MSS             = 1376       // rudp mss size
	// rudp / kcp
	DEFAULT_WRITE_BUF_SIZE = 4 * 1024 * 1024 // rudp write buffer size
	DEFAULT_READ_BUF_SIZE  = 4 * 1024 * 1024 // rudp read buffer size
	DEFAULT_FLUSH_INTERVAL = 10              // rudp flush interval 10 ms default
	MS_TO_NS               = 1000000
	S_TO_NS                = 1000000000
	MB_TO_B                = 1024 * 1024
	KB_TO_B                = 1024
	ACCEPT_SIGNAL          = 1
)

const (
	TCP_INTERVAL_HEADER       = "[ ID]    Interval        Transfer        Bandwidth        RTT        Retrans\n"
	TCP_RESULT_HEADER         = "[ ID]    Interval        Transfer        Bandwidth        RTT        Retrans   Retrans(%%)\n"
	RUDP_INTERVAL_HEADER      = "[ ID]    Interval        Transfer        Bandwidth        RTT        Retrans   Retrans(%%)  Lost(%%)  Early(%%)  Fast(%%)\n"
	RUDP_RESULT_HEADER        = "[ ID]    Interval        Transfer        Bandwidth        RTT        Retrans   Retrans(%%)  Lost(%%)  Early(%%)  Fast(%%)  Recover(%%)  PktsLost(%%)  SegsLost(%%)\n"
	TCP_REPORT_SINGLE_STREAM  = "[  %v] %4.2f-%4.2f sec\t%5.2f MB\t%5.2f Mb/s\t%6.1fms\t%4v\n"
	RUDP_REPORT_SINGLE_STREAM = "[  %v] %4.2f-%4.2f sec\t%5.2f MB\t%5.2f Mb/s\t%6.1fms\t%4v\t%2.2f%%\t%2.2f%%\t%2.2f%%\t%2.2f%%\n"
	TCP_REPORT_SINGLE_RESULT  = "[  %v] %4.2f-%4.2f sec\t%5.2f MB\t%5.2f Mb/s\t%6.1fms\t%4v\t%2.2f%%\t[%s]\n"
	RUDP_REPORT_SINGLE_RESULT = "[  %v] %4.2f-%4.2f sec\t%5.2f MB\t%5.2f Mb/s\t%6.1fms\t%4v\t%2.2f%%\t%2.2f%%\t%2.2f%%\t%2.2f%%\t%2.2f%%\t%2.2f%%\t%2.2f%%\t[%s]\n"
	REPORT_SUM_STREAM         = "[SUM] %4.2f-%4.2f sec\t%5.2f MB\t%5.2f Mb/s\t%6.1fms\t%4v\t\n"
	REPORT_SEPERATOR          = "- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - -\n"
	SUMMARY_SEPERATOR         = "- - - - - - - - - - - - - - - - SUMMARY - - - - - - - - - - - - - - - -\n"
)

type IperfTest struct {
	mu        sync.Mutex
	isServer  bool
	mode      bool // true for sender. false for receiver
	reverse   bool // server send?
	addr      string
	port      uint
	state     uint
	duration  uint // sec
	noDelay   bool
	interval  uint // ms
	proto     protocol
	protocols []protocol

	/* stream */

	listener      net.Listener
	protoListener net.Listener
	ctrlConn      net.Conn
	ctrlChan      chan uint
	setting       *iperfSetting
	streamNum     uint
	streams       []*iperfStream

	/* test statistics */
	bytesReceived  uint64
	blocksReceived uint64
	bytesSent      uint64
	blocksSent     uint64
	done           bool

	/* timer */
	timer ITimer
	//omit_timer 		ITimer  // not used yet
	statsTicker  ITicker
	reportTicker ITicker
	chStats      chan bool

	/* call back function */

	statsCallback    func(test *IperfTest)
	reporterCallback func(test *IperfTest)
	//on_new_stream 	on_new_stream_callback
	//on_test_start 	on_test_start_callback
	//on_connect 		on_connect_callback
	//on_test_finish 	on_test_finish_callback
}

// output_callback is a prototype which ought capture conn and call conn.Write
//type on_new_stream_callback func(test *iperf_stream)
//type on_test_start_callback func(test *iperf_test)
//type on_connect_callback func(test *iperf_test)
//type on_test_finish_callback func(test *iperf_test)

type protocol interface {
	//name string
	name() string
	accept(test *IperfTest) (net.Conn, error)
	listen(test *IperfTest) (net.Listener, error)
	connect(test *IperfTest) (net.Conn, error)
	send(test *iperfStream) int
	recv(test *iperfStream) int
	// init will be called before send/recv data
	init(test *IperfTest) int
	// teardown will be called before send/recv data
	teardown(test *IperfTest) int
	// statsCallback will be invoked intervally, please get some other statistics in this function
	statsCallback(test *IperfTest, sp *iperfStream, tempResult *iperf_interval_results) int
}

type iperfStream struct {
	role       int //SENDER_STREAM or RECEIVE_STREAM
	test       *IperfTest
	result     *iperf_stream_results
	canSend    bool
	conn       net.Conn
	sendTicker ITicker

	buffer []byte //buffer to send

	rcv func(sp *iperfStream) int // return recv size. -1 represent EOF.
	snd func(sp *iperfStream) int // return send size. -1 represent socket close.

}

type iperfSetting struct {
	blksize    uint
	burst      bool // burst & rate & pacingTime should be set at the same time
	rate       uint // bit per second
	pacingTime uint // ms
	bytes      uint64
	blocks     uint64

	// rudp only
	sndWnd        uint
	rcvWnd        uint
	readBufSize   uint // bit
	writeBufSize  uint // bit
	flushInterval uint // ms
	noCong        bool // bbr or not?
	fastResend    uint
	dataShards    uint // for fec
	parityShards  uint
}

// params to exchange
// tips: all the members should be visible, or json decoder cannot encode it
type stream_params struct {
	ProtoName     string
	Reverse       bool
	Duration      uint
	NoDelay       bool
	Interval      uint
	StreamNum     uint
	Blksize       uint
	SndWnd        uint
	RcvWnd        uint
	ReadBufSize   uint
	WriteBufSize  uint
	FlushInterval uint
	NoCong        bool
	FastResend    uint
	DataShards    uint
	ParityShards  uint
	Burst         bool
	Rate          uint
	PacingTime    uint
}

func (p stream_params) String() string {
	s := fmt.Sprintf("name:%v\treverse:%v\tdur:%v\tno_delay:%v\tinterval:%v\tstream_num:%v\tBlkSize:%v\tSndWnd:%v\tRcvWnd:%v\tNoCong:%v\tBurst:%v\tDataShards:%v\tParityShards:%v\t",
		p.ProtoName, p.Reverse, p.Duration, p.NoDelay, p.Interval, p.StreamNum, p.Blksize, p.SndWnd, p.RcvWnd, p.NoCong, p.Burst, p.DataShards, p.ParityShards)
	return s
}

type iperf_stream_results struct {
	bytes_received                  uint64
	bytes_sent                      uint64
	bytes_received_this_interval    uint64
	bytes_sent_this_interval        uint64
	bytes_sent_omit                 uint64
	stream_retrans                  uint
	stream_prev_total_retrans       uint
	stream_lost                     uint
	stream_prev_total_lost          uint
	stream_early_retrans            uint
	stream_prev_total_early_retrans uint
	stream_fast_retrans             uint
	stream_prev_total_fast_retrans  uint
	stream_recovers                 uint
	stream_in_segs                  uint
	stream_in_pkts                  uint
	stream_out_segs                 uint
	stream_out_pkts                 uint
	stream_max_rtt                  uint
	stream_min_rtt                  uint
	stream_sum_rtt                  uint // micro sec
	stream_cnt_rtt                  uint
	stream_repeat_segs              uint
	start_time                      time.Time
	end_time                        time.Time
	start_time_fixed                time.Time
	interval_results                []iperf_interval_results
}

type stream_results_array []stream_results_exchange

// result to exchange
// tips: all the members should be visible, or json decoder cannot encode it
type stream_results_exchange struct {
	Id        uint
	Bytes     uint64
	Retrans   uint
	Jitter    uint
	InPkts    uint
	OutPkts   uint
	InSegs    uint
	OutSegs   uint
	Recovered uint
	StartTime time.Time
	EndTime   time.Time
}

func (r stream_results_exchange) String() string {
	s := fmt.Sprintf("id:%v\tbytes:%v\tretrans:%v\tjitter:%v\tInPkts:%v\tOutPkts:%v\tInSegs:%v\tOutSegs:%v\tstart_time:%v\tend_time:%v\t",
		r.Id, r.Bytes, r.Retrans, r.Jitter, r.InPkts, r.OutPkts, r.InSegs, r.OutSegs, r.StartTime, r.EndTime)
	return s
}

type iperf_interval_results struct {
	bytes_transfered       uint64
	interval_start_time    time.Time
	interval_end_time      time.Time
	interval_dur           time.Duration
	rtt                    uint // us
	rto                    uint // us
	interval_lost          uint
	interval_early_retrans uint
	interval_fast_retrans  uint
	interval_retrans       uint // segs num
	/* for udp */
	interval_packet_cnt uint
	omitted             uint
}

func (test *IperfTest) setDone() {
	test.mu.Lock()
	defer test.mu.Unlock()
	test.done = true
}

func (test *IperfTest) isDone() bool {
	test.mu.Lock()
	defer test.mu.Unlock()
	return test.done
}