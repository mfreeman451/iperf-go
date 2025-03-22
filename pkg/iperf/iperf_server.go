package iperf

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

func (test *IperfTest) serverListen() int {
	listenAddr := ":"
	listenAddr += strconv.Itoa(int(test.port))

	var err error

	test.listener, err = net.Listen("tcp", listenAddr)

	if err != nil {
		return -1
	}
	fmt.Printf("Server listening on %v\n", test.port)

	return 0
}

func (test *IperfTest) handleServerCtrlMsg() {
	buf := make([]byte, 4) // only for ctrl state

	for {
		Log.Debugf("handleServerCtrlMsg waiting for control message")
		n, err := test.ctrlConn.Read(buf)
		if err != nil {
			Log.Errorf("Ctrl conn read failed: %v", err)
			if err == io.EOF || strings.Contains(err.Error(), "use of closed network connection") {
				Log.Infof("Client control connection closed")
				test.mu.Lock()
				test.state = IPERF_DONE
				test.mu.Unlock()
				test.ctrlChan <- IPERF_DONE
				return
			}
			// Let defer in test handle closure to avoid double-close
			test.mu.Lock()
			test.state = SERVER_TERMINATE // Indicate error termination
			test.mu.Unlock()
			test.ctrlChan <- SERVER_TERMINATE
			return
		}

		state := binary.LittleEndian.Uint32(buf[:])
		Log.Debugf("Ctrl conn received n = %v state = [%v]", n, state)

		test.mu.Lock()
		test.state = uint(state)
		test.mu.Unlock()

		Log.Infof("Server Enter state %v", state)

		switch state {
		case TEST_START:
			Log.Debugf("Received TEST_START")
		case TEST_END:
			Log.Infof("Server Enter Test End state...")
			test.mu.Lock()
			test.done = true
			test.mu.Unlock()
			if test.statsCallback != nil {
				test.statsCallback(test)
			}
			test.closeAllStreams()
			if test.setSendState(IPERF_EXCHANGE_RESULT) < 0 {
				Log.Errorf("set_send_state error for IPERF_EXCHANGE_RESULT")
				test.mu.Lock()
				test.state = SERVER_TERMINATE
				test.mu.Unlock()
				test.ctrlChan <- SERVER_TERMINATE
				return
			}
			Log.Infof("Server Enter Exchange Result state...")
			if test.exchangeResults() < 0 {
				Log.Errorf("exchangeResults failed")
				test.mu.Lock()
				test.state = SERVER_TERMINATE
				test.mu.Unlock()
				test.ctrlChan <- SERVER_TERMINATE
				return
			}
			if test.setSendState(IPERF_DISPLAY_RESULT) < 0 {
				Log.Errorf("set_send_state error for IPERF_DISPLAY_RESULT")
				test.mu.Lock()
				test.state = SERVER_TERMINATE
				test.mu.Unlock()
				test.ctrlChan <- SERVER_TERMINATE
				return
			}
			Log.Infof("Server Enter Display Result state...")
			if test.reporterCallback != nil {
				test.reporterCallback(test)
			}
		case IPERF_DONE:
			Log.Debugf("Server reached IPERF_DONE")
			test.proto.teardown(test)
			test.ctrlChan <- IPERF_DONE
			return
		case CLIENT_TERMINATE:
			test.mu.Lock()
			oldState := test.state
			test.state = IPERF_DISPLAY_RESULT
			test.mu.Unlock()
			test.reporterCallback(test)
			test.mu.Lock()
			test.state = oldState
			test.mu.Unlock()
			test.closeAllStreams()
			Log.Infof("Client is terminated.")
			test.mu.Lock()
			test.state = IPERF_DONE
			test.mu.Unlock()
			test.ctrlChan <- IPERF_DONE
			return
		default:
			Log.Errorf("Unexpected situation with state = %v", state)
			test.mu.Lock()
			test.state = SERVER_TERMINATE
			test.mu.Unlock()
			test.ctrlChan <- SERVER_TERMINATE
			return
		}
	}
}

func (test *IperfTest) createServerTimer() int {
	now := time.Now()

	cd := TimerClientData{p: test}

	test.timer = timerCreate(now, serverTimerProc, cd, (test.duration+5)*1000) // convert sec to ms, add 5 sec to ensure client end first

	times := test.duration * 1000 / test.interval

	test.statsTicker = tickerCreate(now, serverStatsTickerProc, cd, test.interval, times-1)
	test.reportTicker = tickerCreate(now, serverReportTickerProc, cd, test.interval, times-1)

	if test.timer.timer == nil || test.statsTicker.ticker == nil || test.reportTicker.ticker == nil {
		Log.Error("timer create failed.")
	}

	return 0
}

func serverTimerProc(data TimerClientData, now time.Time) {
	Log.Debugf("Enter server_timer_proc")

	test := data.p.(*IperfTest)

	if test.done {
		return
	}

	test.done = true

	// close all streams
	for _, sp := range test.streams {
		err := sp.conn.Close()
		if err != nil {
			Log.Errorf("Close stream conn failed. err = %v", err)

			return
		}
	}

	test.timer.done <- true
	//test.ctrl_conn.Close()		//  ctrl conn should be closed at last
	//log.Infof("Server exceed duration. Close control connection.")
}

func serverStatsTickerProc(data TimerClientData, now time.Time) {
	test := data.p.(*IperfTest)

	if test.done {
		return
	}

	if test.statsCallback != nil {
		test.statsCallback(test)
	}
}

func serverReportTickerProc(data TimerClientData, now time.Time) {
	test := data.p.(*IperfTest)

	if test.done {
		return
	}

	if test.reporterCallback != nil {
		test.reporterCallback(test)
	}
}

func (test *IperfTest) createServerOmitTimer() int {
	// undo, depend on which kind of timer
	return 0
}

func (test *IperfTest) runServer(wg *sync.WaitGroup) int {
	Log.Debugf("Enter run_server")
	if test.serverListen() < 0 {
		Log.Error("Listen failed")
		return -1
	}

	fmt.Printf("Server listening on %v\n", test.port)

	test.mu.Lock()
	test.state = IPERF_START
	test.mu.Unlock()

	Log.Info("Enter Iperf start state...")
	test.ctrlChan <- IPERF_START // Signal that server is ready

	conn, err := test.listener.Accept() // Now safe to block here
	if err != nil {
		Log.Errorf("Accept failed: %v", err)
		return -2
	}

	test.ctrlConn = conn
	err = test.ctrlConn.SetDeadline(time.Now().Add(30 * time.Second))
	if err != nil {
		Log.Errorf("Set deadline failed: %v", err)

		return 0
	}

	fmt.Printf("Accept connection from client: %v\n", conn.RemoteAddr())

	// Exchange params
	if test.setSendState(IPERF_EXCHANGE_PARAMS) < 0 {
		Log.Error("set_send_state error.")
		return -3
	}

	Log.Info("Enter Exchange Params state...")
	if test.exchangeParams() < 0 {
		Log.Errorf("exchange params failed: %v", err)
		return -3
	}

	go test.handleServerCtrlMsg() // Coroutine to handle control messages

	if test.isServer {
		listener, err := test.proto.listen(test)
		if err != nil {
			Log.Errorf("proto listen error: %v", err)
			return -4
		}
		test.protoListener = listener
		Log.Debugf("Protocol listener established on port %d", test.port)
	}

	// Create streams
	if test.setSendState(IPERF_CREATE_STREAM) < 0 {
		Log.Error("set_send_state error.")
		return -3
	}

	Log.Info("Enter Create Stream state...")

	var isIperfDone = false

	for !isIperfDone {
		select {
		case state := <-test.ctrlChan:
			Log.Debugf("Ctrl channel receive state [%v]", state)

			switch state {
			case IPERF_DONE:
				Log.Infof("Received IPERF_DONE, shutting down server")
				isIperfDone = true
				return 0

			case IPERF_CREATE_STREAM:
				var streamNum uint = 0

				for streamNum < test.streamNum {
					protoConn, err := test.proto.accept(test)
					if err != nil {
						Log.Errorf("proto accept error: %v", err)
						return -4
					}

					streamNum++

					var sp *iperfStream
					if test.mode == IPERF_SENDER {
						sp = test.newStream(protoConn, SENDER_STREAM)
					} else {
						sp = test.newStream(protoConn, RECEIVER_STREAM)
					}

					if sp == nil {
						Log.Error("Create new stream failed.")
						return -4
					}

					test.mu.Lock()
					test.streams = append(test.streams, sp)
					test.mu.Unlock()

					Log.Debugf("Created stream %d of %d", streamNum, test.streamNum)
				}

				if streamNum == test.streamNum {
					Log.Infof("All %d streams created successfully", streamNum)
					if wg != nil {
						wg.Done() // Signal that all streams are created
						wg = nil  // Prevent multiple Done() calls
						Log.Debugf("Signaled WaitGroup completion")
					}

					if test.setSendState(TEST_START) != 0 {
						Log.Errorf("set_send_state error for TEST_START")
						return -5
					}

					Log.Info("Enter Test Start state...")

					if test.initTest() < 0 {
						Log.Errorf("Init test failed.")
						return -5
					}

					if test.createServerTimer() < 0 {
						Log.Errorf("Create Server timer failed.")
						return -6
					}

					if test.createServerOmitTimer() < 0 {
						Log.Errorf("Create Server omit timer failed.")
						return -7
					}

					if test.mode == IPERF_SENDER {
						if rtn := test.createSenderTicker(); rtn < 0 {
							Log.Errorf("create_sender_ticker failed. rtn = %v", rtn)
							return -7
						}
					}

					if test.setSendState(TEST_RUNNING) != 0 {
						Log.Errorf("set_send_state error for TEST_RUNNING")
						return -8
					}
				}

			case TEST_RUNNING:
				// Regular mode. Server receives or sends based on mode.
				Log.Info("Enter Test Running state...")

				for i, sp := range test.streams {
					if sp.role == SENDER_STREAM {
						go sp.iperfSend(test)
						Log.Infof("Server Stream %d start sending.", i)
					} else {
						go sp.iperfRecv(test)
						Log.Infof("Server Stream %d start receiving.", i)
					}
				}

				Log.Info("Server all streams started...")

			case TEST_END:
				Log.Debugf("Received TEST_END, continuing to wait for next state")
				continue

			default:
				Log.Debugf("Channel unhandled state [%v]", state)
			}
		}
	}

	Log.Debugf("Server side done.")
	return 0
}
