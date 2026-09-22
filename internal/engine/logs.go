package engine

import (
	"bytes"
	"encoding/binary"
	"io"
)

const logHeaderLen = 8

func DemuxLogs(b []byte) string {
	var out bytes.Buffer
	for len(b) >= logHeaderLen {
		if b[0] > 2 || b[1] != 0 || b[2] != 0 || b[3] != 0 {
			out.Write(b)
			return out.String()
		}
		size := int(binary.BigEndian.Uint32(b[4:logHeaderLen]))
		b = b[logHeaderLen:]
		if size > len(b) {
			size = len(b)
		}
		out.Write(b[:size])
		b = b[size:]
	}
	out.Write(b)
	return out.String()
}

func LogWriter(dst io.Writer, src io.Reader) error {
	header := make([]byte, logHeaderLen)
	buf := make([]byte, 32<<10)
	for {
		n, err := io.ReadFull(src, header)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if n > 0 {
				_, _ = dst.Write(header[:n])
			}
			return nil
		}
		if err != nil {
			return err
		}
		if header[0] > 2 || header[1] != 0 || header[2] != 0 || header[3] != 0 {
			if _, err := dst.Write(header); err != nil {
				return err
			}
			_, err := io.Copy(dst, src)
			return err
		}
		size := int(binary.BigEndian.Uint32(header[4:]))
		for size > 0 {
			chunk := size
			if chunk > len(buf) {
				chunk = len(buf)
			}
			n, err := io.ReadFull(src, buf[:chunk])
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if err != nil {
				if err == io.EOF || err == io.ErrUnexpectedEOF {
					return nil
				}
				return err
			}
			size -= n
		}
	}
}

func LastLines(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	lines := bytes.Split([]byte(s), []byte("\n"))
	for len(lines) > 0 && len(bytes.TrimSpace(lines[len(lines)-1])) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}
