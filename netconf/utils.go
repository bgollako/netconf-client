package netconf

import (
	"bytes"
	"io"
	"time"
)

const (
	ackOk  = "<ack>ok</ack>"
	ackErr = "<ack>error</ack>"

	defaultHelloSize        = 4096
	defaultSwitchDelay      = 2 * time.Second
	defaultWriteChannelSize = 10

	delimiter_1_0 = "]]>]]>"
	delimiter_1_1 = "\n##\n"
	suffix1_1     = "#%d\n"

	tag_notification = "<notification"
	tag_rpc_error    = "<rpc-error"
	tag_rpc_reply    = "<rpc-reply"

	capabilities_1_0_1_1 = `<?xml version="1.0" encoding="UTF-8"?>
<hello xmlns="urn:ietf:params:xml:ns:netconf:base:1.0">
<capabilities>
<capability>urn:ietf:params:netconf:base:1.0</capability>
<capability>urn:ietf:params:netconf:base:1.1</capability>
</capabilities>
</hello>
]]>]]>
`
)

// Reads till the delimiter has been encountered.
// This function has been derivied from ioutil.ReadAll which provides a very
// memory efficient way of reading from a reader
func ReadTillDelimiter(reader io.Reader, leftovers, delimiter []byte) ([]byte, []byte, error) {
	b := make([]byte, 0, 512)
	for {
		n, err := reader.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if err != nil {
			return b, nil, err
		}

		if end := bytes.Index(b, delimiter); end != -1 {
			end = end + len(delimiter)
			return b[:end], b[end:], nil
		}

		if len(b) == cap(b) {
			// Add more capacity (let append pick how much).
			b = append(b, 0)[:len(b)]
		}
	}
}
