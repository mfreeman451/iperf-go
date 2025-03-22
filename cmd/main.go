package main

import "iperf-go/pkg/iperf"

/*
	Possible Pitfalls:

	The big-endian/little-endian issue has not been considered, which may cause problems.
	Currently, the default is to use little-endian (following the system).
*/

func main() {
	test := iperf.NewIperfTest()
	if test == nil {
		iperf.Log.Error("create new test error")
	}

	test.Init()

	if rtn := test.ParseArguments(); rtn < 0 {
		iperf.Log.Errorf("parse arguments error: %v", rtn)
	}

	if rtn := test.RunTest(nil); rtn < 0 {
		iperf.Log.Errorf("run test failed: %v", rtn)
	}

	test.FreeTest()
}
