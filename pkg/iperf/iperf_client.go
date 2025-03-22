package iperf

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

func (test *IperfTest) createStreams() int {
	test.mu.Lock()
	defer test.mu.Unlock()

	for i := uint(0); i < test.streamNum; i++ {
		conn, err := test.proto.connect(test)
		if err != nil {
			Log.Errorf("Connect failed. err = %v", err)
			return -1
		}

		var sp *iperfStream
		if test.mode == IPERF_SENDER {
			sp = test.newStream(conn, SENDER_STREAM)
		} else {
			sp = test.newStream(conn, RECEIVER_STREAM)
		}

		test.streams = append(test.streams, sp)
	}

	return 0
}

func (test *IperfTest) createClientTimer() int {
	now := time.Now()
	test.timer = test.timerCreate(now, test.duration*1000) // convert sec to ms
	times := test.duration * 1000 / test.interval
	test.statsTicker = test.tickerCreate(now, test.interval, times-1)
	test.reportTicker = test.tickerCreate(now, test.interval, times-1)

	if test.timer.timer == nil || test.statsTicker.ticker == nil || test.reportTicker.ticker == nil {
		Log.Error("timer create failed.")
		return -1
	}

	return 0
}

func (test *IperfTest) timerCreate(now time.Time, dur uint /* in ms */) ITimer {
	realDur := time.Now().Sub(now) + time.Duration(dur)*time.Millisecond
	timer := time.NewTimer(realDur)
	done := make(chan bool, 1)

	go func() {
		defer timer.Stop()
		for {
			select {
			case <-done:
				Log.Debugf("Timer recv done. dur: %v", dur)
				return
			case t := <-timer.C:
				test.clientTimerProc(t)
			}
		}
	}()

	return ITimer{timer: timer, done: done}
}

func (test *IperfTest) tickerCreate(now time.Time, interval, maxTimes uint /* in ms */) ITicker {
	ticker := time.NewTicker(time.Duration(interval) * time.Millisecond)
	done := make(chan bool, 1)

	go func() {
		var cnt uint
		defer ticker.Stop()
		for {
			select {
			case <-done:
				Log.Debugf("Ticker recv done. interval:%v", interval)
				return
			case t := <-ticker.C:
				if cnt >= maxTimes {
					return
				}
				if cnt%2 == 0 {
					test.clientStatsTickerProc(t)
				} else {
					test.clientReportTickerProc(t)
				}
				cnt++
			}
		}
	}()

	return ITicker{ticker: ticker, done: done}
}

func (test *IperfTest) clientTimerProc(now time.Time) {
	Log.Debugf("Enter client_timer_proc")
	test.mu.Lock()
	test.timer.done <- true
	test.done = true // Protected write
	test.mu.Unlock()
	test.timer.timer = nil
}

func (test *IperfTest) clientStatsTickerProc(now time.Time) {
	test.mu.Lock()
	if test.done { // Protected read
		test.mu.Unlock()
		return
	}
	test.mu.Unlock()
	if test.statsCallback != nil {
		test.statsCallback(test)
	}
}

func (test *IperfTest) clientReportTickerProc(now time.Time) {
	test.mu.Lock()
	if test.done { // Protected read
		test.mu.Unlock()
		return
	}
	test.mu.Unlock()
	if test.reporterCallback != nil {
		test.reporterCallback(test)
	}
}

func (test *IperfTest) createClientOmitTimer() int {
	// TODO: Implement if needed
	return 0
}

func (test *IperfTest) clientEnd() {
	Log.Debugf("Enter client_end")
	for _, sp := range test.streams {
		if err := sp.conn.Close(); err != nil {
			Log.Errorf("Stream close failed. err = %v", err)
			return
		}
	}

	if test.reporterCallback != nil {
		test.reporterCallback(test)
	}

	test.proto.teardown(test)
	if test.setSendState(IPERF_DONE) < 0 {
		Log.Errorf("set_send_state failed")
	}

	Log.Infof("Client Enter IPerf Done...")

	if test.ctrlConn != nil {
		if err := test.ctrlConn.Close(); err != nil {
			Log.Errorf("Ctrl conn close failed. err = %v", err)
			return
		}
		test.ctrlChan <- IPERF_DONE // Ensure main loop exits
	}
}

func (test *IperfTest) handleClientCtrlMsg() {
	buf := make([]byte, 4)

	for {
		test.mu.Lock()
		if test.state == IPERF_DONE {
			test.mu.Unlock()
			return
		}
		test.mu.Unlock()

		Log.Debugf("handleClientCtrlMsg waiting for state")
		test.ctrlConn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := io.ReadFull(test.ctrlConn, buf)
		test.ctrlConn.SetReadDeadline(time.Time{}) // Clear deadline

		if err != nil {
			if err == io.EOF || strings.Contains(err.Error(), "use of closed network connection") {
				Log.Infof("Server control connection closed")
				test.mu.Lock()
				test.state = IPERF_DONE
				test.ctrlChan <- IPERF_DONE
				test.mu.Unlock()
				return
			}
			Log.Errorf("ctrl_conn read failed. err=%T, %v", err, err)
			test.mu.Lock()
			test.state = IPERF_DONE
			test.ctrlChan <- IPERF_DONE
			test.mu.Unlock()
			return
		}

		state := binary.LittleEndian.Uint32(buf[:n])
		Log.Debugf("Client recv %d bytes, data = %x, state = %v", n, buf[:n], state)

		// Send acknowledgment
		ack := make([]byte, 4)
		binary.LittleEndian.PutUint32(ack, ACK_SIGNAL)
		an, err := test.ctrlConn.Write(ack)
		if err != nil || an != 4 {
			Log.Errorf("Failed to send acknowledgment: %v, bytes sent: %d", err, an)
			test.mu.Lock()
			test.state = IPERF_DONE
			test.ctrlChan <- IPERF_DONE
			test.mu.Unlock()
			return
		}
		Log.Debugf("Sent acknowledgment bytes = %x for state = %v", ack, state)

		Log.Debugf("Sent acknowledgment %x for state = %v", ACK_SIGNAL, state)

		test.mu.Lock()
		test.state = uint(state)
		test.mu.Unlock()

		Log.Infof("Client Enter %v state...", state)
		switch state {
		case IPERF_EXCHANGE_PARAMS:
			if rtn := test.exchangeParams(); rtn < 0 {
				Log.Errorf("exchange_params failed: %v", rtn)
				test.mu.Lock()
				test.ctrlChan <- IPERF_DONE
				test.mu.Unlock()
				return
			}
		case IPERF_CREATE_STREAM:
			Log.Debugf("Received IPERF_CREATE_STREAM, setting up protocol listener and streams")
			test.mu.Lock()
			if len(test.streams) > 0 {
				Log.Debugf("Streams already created, skipping")
				test.mu.Unlock()
				break
			}
			test.mu.Unlock()
			if rtn := test.createStreams(); rtn < 0 {
				Log.Errorf("create_streams failed: %v", rtn)
				test.mu.Lock()
				test.ctrlChan <- IPERF_DONE
				test.mu.Unlock()
				return
			}
		case TEST_START:
			if rtn := test.initTest(); rtn < 0 {
				Log.Errorf("init_test failed: %v", rtn)
				test.mu.Lock()
				test.ctrlChan <- IPERF_DONE
				test.mu.Unlock()
				return
			}
			if rtn := test.createClientTimer(); rtn < 0 {
				Log.Errorf("create_client_timer failed: %v", rtn)
				test.mu.Lock()
				test.ctrlChan <- IPERF_DONE
				test.mu.Unlock()
				return
			}
			if rtn := test.createClientOmitTimer(); rtn < 0 {
				Log.Errorf("create_client_omit_timer failed: %v", rtn)
				test.mu.Lock()
				test.ctrlChan <- IPERF_DONE
				test.mu.Unlock()
				return
			}
			if test.mode == IPERF_SENDER {
				if rtn := test.createSenderTicker(); rtn < 0 {
					Log.Errorf("create_sender_ticker failed: %v", rtn)
					test.mu.Lock()
					test.ctrlChan <- IPERF_DONE
					test.mu.Unlock()
					return
				}
			}
			test.mu.Lock()
			test.ctrlChan <- TEST_RUNNING
			test.mu.Unlock()
		case TEST_RUNNING:
			test.mu.Lock()
			test.ctrlChan <- TEST_RUNNING
			test.mu.Unlock()
		case IPERF_EXCHANGE_RESULT:
			if rtn := test.exchangeResults(); rtn < 0 {
				Log.Errorf("exchange_results failed: %v", rtn)
				test.mu.Lock()
				test.ctrlChan <- IPERF_DONE
				test.mu.Unlock()
				return
			}
		case IPERF_DISPLAY_RESULT:
			test.clientEnd()
		case IPERF_DONE:
			test.mu.Lock()
			test.ctrlChan <- IPERF_DONE
			test.mu.Unlock()
			return
		case SERVER_TERMINATE:
			test.mu.Lock()
			oldState := test.state
			test.state = IPERF_DISPLAY_RESULT
			test.mu.Unlock()
			test.reporterCallback(test)
			test.mu.Lock()
			test.state = oldState
			test.mu.Unlock()
		default:
			Log.Infof("Ignoring unexpected state = %v", state)
		}
	}
}

func (test *IperfTest) ConnectServer() int {
	tcpAddr, err := net.ResolveTCPAddr("tcp4", test.addr+":"+strconv.Itoa(int(test.port)))
	if err != nil {
		Log.Errorf("Resolve TCP Addr failed. err = %v, addr = %v", err, test.addr+strconv.Itoa(int(test.port)))
		return -1
	}

	conn, err := net.DialTCP("tcp", nil, tcpAddr)
	if err != nil {
		Log.Errorf("Connect TCP Addr failed. err = %v, addr = %v", err, test.addr+strconv.Itoa(int(test.port)))
		return -1
	}

	test.ctrlConn = conn
	fmt.Printf("Connect to server %v succeed.\n", test.addr+":"+strconv.Itoa(int(test.port)))
	return 0
}

func (test *IperfTest) runClient() int {
	Log.Infof("runClient starting")
	if test.ctrlConn == nil { // Only connect if not already connected
		rtn := test.ConnectServer()
		if rtn < 0 {
			Log.Errorf("ConnectServer failed: %d", rtn)
			return -1
		}
		test.ctrlConn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	go test.handleClientCtrlMsg()

	var isIperfDone bool
	var testEndNum uint

	for !isIperfDone {
		select {
		case state := <-test.ctrlChan:
			Log.Debugf("runClient received state [%v]", state)
			if state == TEST_RUNNING {
				Log.Info("Client enter Test Running state...")
				for i, sp := range test.streams {
					if sp.role == SENDER_STREAM {
						go sp.iperfSend(test)
						Log.Infof("Client Stream %v start sending.", i)
					} else {
						go sp.iperfRecv(test)
						Log.Infof("Client Stream %v start receiving.", i)
					}
				}
				Log.Info("Create all streams finish...")
			} else if state == TEST_END {
				testEndNum++
				if testEndNum < test.streamNum || testEndNum == test.streamNum+1 {
					continue
				} else if testEndNum > test.streamNum+1 {
					Log.Errorf("Receive more TEST_END signal than expected")
					return -1
				}
				Log.Infof("Client all Stream closed.")
				test.mu.Lock()
				test.done = true
				test.mu.Unlock()
				if test.statsCallback != nil {
					test.statsCallback(test)
				}
				if test.setSendState(TEST_END) < 0 {
					Log.Errorf("set_send_state failed: %v", TEST_END)
					return -1
				}
				Log.Info("Client Enter Test End State.")
			} else if state == IPERF_DONE {
				isIperfDone = true
			} else {
				Log.Debugf("Channel Unhandled state [%v]", state)
			}
		}
	}

	Log.Infof("runClient completed")
	return 0
}

// Leave sendTickerProc as is for now, but it could be refactored later if needed
func sendTickerProc(data TimerClientData, now time.Time) {
	sp := data.p.(*iperfStream)
	sp.test.checkThrottle(sp, now)
}
