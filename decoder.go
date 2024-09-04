package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"regexp"
	"strings"
)

type Decoder interface {
	Decode() (map[string]interface{}, error)
	More() bool
}

type jsonDecoder struct {
	rd  io.Reader
	dec *json.Decoder
}

func newJsonDecoder(r io.Reader) *jsonDecoder {
	d := json.NewDecoder(r)
	d.UseNumber()

	return &jsonDecoder{
		rd:  r,
		dec: d,
	}
}

func (d *jsonDecoder) Decode() (map[string]interface{}, error) {
	m := map[string]interface{}{}
	if err := d.dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

func (d *jsonDecoder) More() bool {
	// len(nil)==0, 此时读到文件末尾，返回io.EOF
	_, err := d.rd.Read(nil)
	return err == nil
}

type zaplogDecoder struct {
	// rd io.Reader
	scanner *bufio.Scanner
}

func newZaplogDecoder(r io.Reader) *zaplogDecoder {
	sc := bufio.NewScanner(r)
	sc.Split(bufio.ScanLines)

	return &zaplogDecoder{
		scanner: sc,
	}
}

var zaplogRE = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}) (TRACE|DEBUG|INFO|WARN|ERROR) (.*.go:\d+) \[.*\] (.*) (\{.*\})$`)

var emptyMap = map[string]interface{}{}

func (d *zaplogDecoder) Decode() (map[string]interface{}, error) {
	for d.scanner.Scan() {
		line := d.scanner.Text()
		line = strings.TrimSpace(line)

		ok := zaplogRE.MatchString(line)
		if !ok {
			log.Printf("warn: invalid log entry - %s", line)
			continue
		} else {
			var m = map[string]interface{}{}

			matches := zaplogRE.FindStringSubmatch(line)
			if len(matches) != 6 {
				log.Printf("warn: invalid log entry - %s", line)
				continue
			}
			// 0: line
			m["datetime"] = matches[1] // 1: datetime
			m["level"] = matches[2]    // 2: level
			m["position"] = matches[3] // 3: position
			m["message"] = matches[4]  // 4: message
			// 5: zapfields
			zapfields := map[string]interface{}{}
			dec := json.NewDecoder(bytes.NewBufferString(matches[5]))
			dec.UseNumber()
			_ = dec.Decode(&zapfields)
			// m["zapfields"] = zapfields
			for k, v := range zapfields {
				m[k] = v
			}

			return m, nil
		}
	}
	return emptyMap, nil
}

func (d *zaplogDecoder) More() bool {
	return d.scanner.Scan()
}
