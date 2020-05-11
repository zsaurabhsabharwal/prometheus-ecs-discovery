package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

type flagMapping map[string]uint8

func (f *flagMapping) String() string {
	bts, err := json.Marshal(f)
	if err != nil {
		return "{\"error\": \"unable to encode\"}"
	}
	return string(bts)
}

func (f *flagMapping) Set(v string) error {
	items := strings.SplitN(v, "=", 2)
	count, err := strconv.ParseUint(items[1], 10, 8)
	if err != nil {
		return err
	}
	(*f)[items[0]] = uint8(count)
	return nil
}
