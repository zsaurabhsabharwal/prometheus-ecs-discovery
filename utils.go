package main

import (
	"encoding/json"
	"hash/fnv"
	"log"
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

func hash(s string, n uint8) uint8 {
	h := fnv.New64a()
	_, err := h.Write([]byte(s))
	if err != nil {
		log.Printf("unable to hash the string %s - %d", s, n)
		return 0
	}
	return uint8(h.Sum64() % uint64(n))
}

func Find(slice []int64, val int64) (int, bool) {
	for i, item := range slice {
		if item == val {
			return i, true
		}
	}
	return -1, false
}
