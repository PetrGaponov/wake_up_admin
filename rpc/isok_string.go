// Code generated by "stringer -type IsOk ./rpc_server.go"; DO NOT EDIT.

package main

import "strconv"

func _() {
	// An "invalid array index" compiler error signifies that the constant values have changed.
	// Re-run the stringer command to generate them again.
	var x [1]struct{}
	_ = x[OK-0]
	_ = x[FAIL-1]
}

const _IsOk_name = "OKFAIL"

var _IsOk_index = [...]uint8{0, 2, 6}

func (i IsOk) String() string {
	if i < 0 || i >= IsOk(len(_IsOk_index)-1) {
		return "IsOk(" + strconv.FormatInt(int64(i), 10) + ")"
	}
	return _IsOk_name[_IsOk_index[i]:_IsOk_index[i+1]]
}