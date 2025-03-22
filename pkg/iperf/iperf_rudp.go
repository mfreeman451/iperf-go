package iperf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	RUDP "github.com/damao33/rudp-go"
	"github.com/op/go-logging"
)

type rudpProto struct {
}

func (r *rudpProto) name() string {
	return RUDP_NAME
}

func (r *rudpProto) accept(test *IperfTest) (net.Conn, error) {
	Log.Debugf("Enter RUDP accept")

	conn, err := test.protoListener.Accept()
	if err != nil {
		return nil, err
	}

	buf := make([]byte, 4)
	n, err := conn.Read(buf)

	signal := binary.LittleEndian.Uint32(buf[:])

	if err != nil || n != 4 || signal != ACCEPT_SIGNAL {
		Log.Errorf("RUDP Receive Unexpected signal")
	}

	Log.Debugf("RUDP accept succeed. signal = %v", signal)

	return conn, nil
}

func (r *rudpProto) listen(test *IperfTest) (net.Listener, error) {
	listener, err := RUDP.ListenWithOptions("0.0.0.0:"+strconv.Itoa(int(test.port)), nil, int(test.setting.dataShards), int(test.setting.parityShards))
	if err != nil {
		return nil, err
	}

	err = listener.SetReadBuffer(int(test.setting.readBufSize))
	if err != nil {
		return nil, err
	} // Safe: err is nil, listener is valid

	err = listener.SetWriteBuffer(int(test.setting.writeBufSize))
	if err != nil {
		return nil, err
	}

	return listener, nil
}

func (r *rudpProto) connect(test *IperfTest) (net.Conn, error) {
	conn, err := RUDP.DialWithOptions(test.addr+":"+strconv.Itoa(int(test.port)), nil, int(test.setting.dataShards), int(test.setting.parityShards))

	if err != nil {
		return nil, err
	}

	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, ACCEPT_SIGNAL)

	n, err := conn.Write(buf)

	if err != nil || n != 4 {
		Log.Errorf("RUDP send accept signal failed")
	}

	Log.Debugf("RUDP connect succeed.")

	return conn, nil
}

func (r *rudpProto) send(sp *iperfStream) int {
	n, err := sp.conn.(*RUDP.UDPSession).Write(sp.buffer)
	if err != nil {
		var serr *net.OpError

		if errors.As(err, &serr) {
			Log.Debugf("r conn already close = %v", serr)

			return -1
		}

		Log.Errorf("r write err = %T %v", err, err)

		return -2
	}

	if n < 0 {
		Log.Errorf("r write err. n = %v", n)

		return n
	}

	sp.result.bytes_sent += uint64(n)
	sp.result.bytes_sent_this_interval += uint64(n)

	//log.Debugf("RUDP send %v bytes of total %v", n, sp.result.bytes_sent)
	return n
}

func (r *rudpProto) recv(sp *iperfStream) int {
	n, err := sp.conn.(*RUDP.UDPSession).Read(sp.buffer)
	if err != nil {
		var serr *net.OpError
		if errors.As(err, &serr) {
			Log.Debugf("r conn already close = %v", serr)
			return -1
		}
		Log.Errorf("r recv err = %T %v", err, err)
		return -2
	}
	if n < 0 {
		return n
	}
	sp.mu.Lock() // Lock before updating shared fields
	if sp.test.state == TEST_RUNNING {
		sp.result.bytes_received += uint64(n)
		sp.result.bytes_received_this_interval += uint64(n)
	}
	sp.mu.Unlock() // Unlock after updates
	return n
}

func (r *rudpProto) init(test *IperfTest) int {
	for _, sp := range test.streams {
		if err := sp.conn.(*RUDP.UDPSession).SetReadBuffer(int(test.setting.readBufSize)); err != nil {
			Log.Infof("r set read buffer failed (non-critical): %v", err)
		}
		if err := sp.conn.(*RUDP.UDPSession).SetWriteBuffer(int(test.setting.writeBufSize)); err != nil {
			Log.Infof("r set write buffer failed (non-critical): %v", err)
		}

		sp.conn.(*RUDP.UDPSession).SetWindowSize(int(test.setting.sndWnd), int(test.setting.rcvWnd))
		sp.conn.(*RUDP.UDPSession).SetStreamMode(true)

		err := sp.conn.(*RUDP.UDPSession).SetDSCP(46)
		if err != nil {
			Log.Errorf("r set dscp failed (non-critical). err = %v", err)
		}

		sp.conn.(*RUDP.UDPSession).SetMtu(1400)
		sp.conn.(*RUDP.UDPSession).SetACKNoDelay(false)

		err = sp.conn.(*RUDP.UDPSession).SetDeadline(time.Now().Add(time.Minute))
		if err != nil {
			Log.Errorf("r set deadline failed. err = %v", err)

			return 0
		}

		var noDelay, resend, nc int

		if test.noDelay {
			noDelay = 1
		} else {
			noDelay = 0
		}

		if test.setting.noCong {
			nc = 1
		} else {
			nc = 0
		}

		resend = int(test.setting.fastResend)

		sp.conn.(*RUDP.UDPSession).SetNoDelay(noDelay, int(test.setting.flushInterval), resend, nc)
	}

	return 0
}

var snmpMu sync.Mutex

func (r *rudpProto) statsCallback(test *IperfTest, sp *iperfStream, tempResult *iperf_interval_results) int {
	// Lock to prevent race conditions when accessing SNMP data
	snmpMu.Lock()
	defer snmpMu.Unlock()

	rp := sp.result

	totalRetrans := uint(RUDP.DefaultSnmp.RetransSegs)
	totalLost := uint(RUDP.DefaultSnmp.LostSegs)
	totalEarlyRetrans := uint(RUDP.DefaultSnmp.EarlyRetransSegs)
	totalFastRetrans := uint(RUDP.DefaultSnmp.FastRetransSegs)
	totalRecovers := uint(RUDP.DefaultSnmp.FECRecovered)

	totalInPkts := uint(RUDP.DefaultSnmp.InPkts)
	totalInSegs := uint(RUDP.DefaultSnmp.InSegs)
	totalOutPkts := uint(RUDP.DefaultSnmp.OutPkts)
	totalOutSegs := uint(RUDP.DefaultSnmp.OutSegs)

	repeatSegs := uint(RUDP.DefaultSnmp.RepeatSegs)

	// retrans
	tempResult.interval_retrans = totalRetrans - rp.stream_prev_total_retrans
	rp.stream_retrans += tempResult.interval_retrans
	rp.stream_prev_total_retrans = totalRetrans

	// lost
	tempResult.interval_lost = totalLost - rp.stream_prev_total_lost
	rp.stream_lost += tempResult.interval_lost
	rp.stream_prev_total_lost = totalLost

	// early retrans
	tempResult.interval_early_retrans = totalEarlyRetrans - rp.stream_prev_total_early_retrans
	rp.stream_early_retrans += tempResult.interval_early_retrans
	rp.stream_prev_total_early_retrans = totalEarlyRetrans

	// fast retrans
	tempResult.interval_fast_retrans = totalFastRetrans - rp.stream_prev_total_fast_retrans
	rp.stream_fast_retrans += tempResult.interval_fast_retrans
	rp.stream_prev_total_fast_retrans = totalFastRetrans

	// recover
	rp.stream_recovers = totalRecovers

	// packets receive
	rp.stream_in_pkts = totalInPkts
	rp.stream_out_pkts = totalOutPkts

	// segs receive
	rp.stream_in_segs = totalInSegs
	rp.stream_out_segs = totalOutSegs
	rp.stream_repeat_segs = repeatSegs

	tempResult.rto = uint(sp.conn.(*RUDP.UDPSession).GetRTO() * 1000)
	tempResult.rtt = uint(sp.conn.(*RUDP.UDPSession).GetSRTTVar() * 1000) // ms to micro sec

	if rp.stream_min_rtt == 0 || tempResult.rtt < rp.stream_min_rtt {
		rp.stream_min_rtt = tempResult.rtt
	}

	if rp.stream_max_rtt == 0 || tempResult.rtt > rp.stream_max_rtt {
		rp.stream_max_rtt = tempResult.rtt
	}

	rp.stream_sum_rtt += tempResult.rtt
	rp.stream_cnt_rtt++

	return 0
}

func (r *rudpProto) teardown(test *IperfTest) int {
	if logging.GetLevel("r") == logging.INFO ||
		logging.GetLevel("r") == logging.DEBUG {

		header := RUDP.DefaultSnmp.Header()
		slices := RUDP.DefaultSnmp.ToSlice()

		for k := range header {
			fmt.Printf("%s: %v\t", header[k], slices[k])
		}

		fmt.Printf("\n")

		if test.setting.noCong == false {
			//RUDP.PrintTracker()
			fmt.Println("TODO::RUDP#PrintTracker()")
		}
		//fmt.Println("TODO:teardown snmp")
	}

	return 0
}
