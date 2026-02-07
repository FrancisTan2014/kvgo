package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"kvgo/protocol"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	var (
		network = flag.String("network", "tcp", "network type: tcp, tcp4, tcp6, unix")
		addr    = flag.String("addr", "127.0.0.1:4000", "server address or unix socket path")
		timeout = flag.Duration("timeout", 5*time.Second, "request timeout")
	)
	flag.Parse()

	conn, err := net.Dial(*network, *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	fmt.Printf("connected to %s\n", *addr)
	fmt.Println("commands: get <key>, put <key> <value>, promote, replicaof <host:port>, cleanup, quit")
	fmt.Println()

	f := protocol.NewConnFramer(conn)
	scanner := bufio.NewScanner(os.Stdin)

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 3)
		cmd := strings.ToLower(parts[0])

		switch cmd {
		case "quit", "exit", "q":
			fmt.Println("bye")
			return

		case "get":
			if len(parts) < 2 {
				fmt.Println("usage: get <key>")
				continue
			}
			key := parts[1]
			if err := doGet(f, key, *timeout); err != nil {
				fmt.Printf("error: %v\n", err)
				if isConnectionError(err) {
					fmt.Println("connection lost, exiting")
					os.Exit(1)
				}
			}

		case "put":
			if len(parts) < 3 {
				fmt.Println("usage: put <key> <value>")
				continue
			}
			key := parts[1]
			value := parts[2]
			if err := doPut(f, key, value, *timeout); err != nil {
				fmt.Printf("error: %v\n", err)
				if isConnectionError(err) {
					fmt.Println("connection lost, exiting")
					os.Exit(1)
				}
			}

		case "promote":
			if err := doPromote(f, *timeout); err != nil {
				fmt.Printf("error: %v\n", err)
				if isConnectionError(err) {
					fmt.Println("connection lost, exiting")
					os.Exit(1)
				}
			}

		case "replicaof":
			if len(parts) < 2 {
				fmt.Println("usage: replicaof <host:port>")
				continue
			}
			primaryAddr := parts[1]
			if err := doReplicaOf(f, primaryAddr, *timeout); err != nil {
				fmt.Printf("error: %v\n", err)
				if isConnectionError(err) {
					fmt.Println("connection lost, exiting")
					os.Exit(1)
				}
			}

		case "cleanup":
			if err := doCleanup(f, *timeout); err != nil {
				fmt.Printf("error: %v\n", err)
				if isConnectionError(err) {
					fmt.Println("connection lost, exiting")
					os.Exit(1)
				}
			}

		default:
			fmt.Printf("unknown command: %s\n", cmd)
			fmt.Println("commands: get <key>, put <key> <value>, promote, replicaof <host:port>, cleanup, quit")
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "error reading input: %v\n", err)
	}
}

func doPromote(f *protocol.Framer, timeout time.Duration) error {
	req := protocol.Request{Cmd: protocol.CmdPromote}
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	if err := f.WriteWithTimeout(payload, timeout); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	respPayload, err := f.ReadWithTimeout(timeout)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	switch resp.Status {
	case protocol.StatusOK:
		fmt.Println("OK (promoted)")
	case protocol.StatusError:
		fmt.Println("(server error)")
	default:
		fmt.Printf("(unexpected status: %d)\n", resp.Status)
	}
	return nil
}

func doGet(f *protocol.Framer, key string, timeout time.Duration) error {
	req := protocol.Request{Cmd: protocol.CmdGet, Key: []byte(key)}
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	if err := f.WriteWithTimeout(payload, timeout); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	respPayload, err := f.ReadWithTimeout(timeout)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	switch resp.Status {
	case protocol.StatusOK:
		fmt.Printf("%s\n", resp.Value)
	case protocol.StatusNotFound:
		fmt.Println("(not found)")
	case protocol.StatusError:
		fmt.Println("(server error)")
	default:
		fmt.Printf("(unknown status: %d)\n", resp.Status)
	}
	return nil
}

func doPut(f *protocol.Framer, key, value string, timeout time.Duration) error {
	req := protocol.Request{Cmd: protocol.CmdPut, Key: []byte(key), Value: []byte(value)}
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	if err := f.WriteWithTimeout(payload, timeout); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	respPayload, err := f.ReadWithTimeout(timeout)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	switch resp.Status {
	case protocol.StatusOK:
		fmt.Println("OK")
	case protocol.StatusError:
		fmt.Println("(server error)")
	default:
		fmt.Printf("(unexpected status: %d)\n", resp.Status)
	}
	return nil
}

func doReplicaOf(f *protocol.Framer, primaryAddr string, timeout time.Duration) error {
	req := protocol.Request{Cmd: protocol.CmdReplicaOf, Value: []byte(primaryAddr)}
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	if err := f.WriteWithTimeout(payload, timeout); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	respPayload, err := f.ReadWithTimeout(timeout)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	switch resp.Status {
	case protocol.StatusOK:
		fmt.Println("OK")
	case protocol.StatusError:
		fmt.Println("(server error)")
	default:
		fmt.Printf("(unexpected status: %d)\n", resp.Status)
	}
	return nil
}

func doCleanup(f *protocol.Framer, timeout time.Duration) error {
	req := protocol.Request{Cmd: protocol.CmdCleanup}
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	if err := f.WriteWithTimeout(payload, timeout); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	respPayload, err := f.ReadWithTimeout(timeout)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	switch resp.Status {
	case protocol.StatusOK:
		fmt.Println("OK (cleanup started)")
	case protocol.StatusError:
		fmt.Println("(server error)")
	default:
		fmt.Printf("(unexpected status: %d)\n", resp.Status)
	}
	return nil
}

// isConnectionError returns true if the error indicates the connection is dead.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	// EOF means server closed the connection.
	if errors.Is(err, io.EOF) {
		return true
	}
	// Check for network errors (connection reset, broken pipe, etc.).
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Also check for common wrapped errors.
	errStr := err.Error()
	return strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "forcibly closed") ||
		strings.Contains(errStr, "EOF")
}
