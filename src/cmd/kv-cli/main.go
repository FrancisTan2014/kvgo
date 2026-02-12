package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"kvgo/protocol"
	"kvgo/transport"
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
	fmt.Println("commands: get [--quorum] <key>, put [--quorum] <key> <value>, promote, replicaof <host:port>, cleanup, quit")
	fmt.Println()

	f := transport.NewConnFramer(conn)
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
				fmt.Println("usage: get [--quorum] <key>")
				continue
			}
			// Parse flags: get --quorum key OR get key --quorum
			var quorum bool
			var key string
			if parts[1] == "--quorum" {
				if len(parts) < 3 {
					fmt.Println("usage: get [--quorum] <key>")
					continue
				}
				quorum = true
				key = parts[2]
			} else {
				key = parts[1]
				if len(parts) > 2 && parts[2] == "--quorum" {
					quorum = true
				}
			}

			if err := doGet(f, key, quorum, *timeout); err != nil {
				fmt.Printf("error: %v\n", err)
				if isConnectionError(err) {
					fmt.Println("connection lost, exiting")
					os.Exit(1)
				}
			}

		case "put":
			if len(parts) < 3 {
				fmt.Println("usage: put [--quorum] <key> <value>")
				continue
			}
			// Parse flags: put --quorum key value OR put key value --quorum
			var quorum bool
			var key, value string
			if parts[1] == "--quorum" {
				if len(parts) < 3 {
					fmt.Println("usage: put [--quorum] <key> <value>")
					continue
				}
				quorum = true
				// Reparse with 4 parts to get key and value
				restParts := strings.SplitN(line, " ", 4)
				if len(restParts) < 4 {
					fmt.Println("usage: put [--quorum] <key> <value>")
					continue
				}
				key = restParts[2]
				value = restParts[3]
			} else {
				key = parts[1]
				value = parts[2]
				// Check for trailing --quorum flag
				restParts := strings.SplitN(line, " ", 4)
				if len(restParts) > 3 && restParts[3] == "--quorum" {
					quorum = true
				}
			}
			_, err := doPut(f, key, value, quorum, *timeout)
			if err != nil {
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
			fmt.Println("commands: get [--quorum] <key>, put [--quorum] <key> <value>, promote, replicaof <host:port>, cleanup, quit")
		}
	}

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "error reading input: %v\n", err)
	}
}

func doPromote(f *transport.Framer, timeout time.Duration) error {
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

func doGet(f *transport.Framer, key string, quorum bool, timeout time.Duration) error {
	req := protocol.Request{Cmd: protocol.CmdGet, Key: []byte(key), RequireQuorum: quorum}
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
		if quorum {
			fmt.Printf("(quorum read: verified across majority, seq=%d)\n", resp.Seq)
		}
	case protocol.StatusNotFound:
		fmt.Println("(not found)")
		if quorum {
			fmt.Printf("(verified not found across majority)\n")
		}
	case protocol.StatusReplicaTooStale:
		// Replica too stale, server suggests redirect to primary
		primaryAddr := string(resp.Value)
		fmt.Printf("(replica too stale, try primary: %s)\n", primaryAddr)
	case protocol.StatusQuorumFailed:
		fmt.Println("(quorum read failed: insufficient replica responses)")
	case protocol.StatusReadOnly:
		// Quorum read timed out or failed, server suggests redirect to primary
		primaryAddr := string(resp.Value)
		fmt.Printf("(replica lagging, try primary: %s)\n", primaryAddr)
	case protocol.StatusError:
		fmt.Println("(server error)")
	default:
		fmt.Printf("(unknown status: %d)\n", resp.Status)
	}
	return nil
}

func doPut(f *transport.Framer, key, value string, quorum bool, timeout time.Duration) (uint64, error) {
	req := protocol.Request{Cmd: protocol.CmdPut, Key: []byte(key), Value: []byte(value), RequireQuorum: quorum}
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		return 0, fmt.Errorf("encode: %w", err)
	}

	if err := f.WriteWithTimeout(payload, timeout); err != nil {
		return 0, fmt.Errorf("write: %w", err)
	}

	respPayload, err := f.ReadWithTimeout(timeout)
	if err != nil {
		return 0, fmt.Errorf("read: %w", err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		return 0, fmt.Errorf("decode: %w", err)
	}

	switch resp.Status {
	case protocol.StatusOK:
		if quorum {
			fmt.Println("OK (quorum write succeeded)")
		} else {
			fmt.Println("OK")
		}
		return resp.Seq, nil // Return seq for read-your-writes tracking
	case protocol.StatusReadOnly:
		// Server is a replica, redirect to primary
		primaryAddr := string(resp.Value)
		fmt.Printf("(replica, redirecting to primary: %s)\n", primaryAddr)
		return doPutToPrimary(key, value, quorum, primaryAddr, timeout)
	case protocol.StatusError:
		fmt.Println("(server error)")
	default:
		fmt.Printf("(unexpected status: %d)\n", resp.Status)
	}
	return 0, nil
}

func doPutToPrimary(key, value string, quorum bool, primaryAddr string, timeout time.Duration) (uint64, error) {
	// Connect to primary and retry
	conn, err := net.DialTimeout("tcp", primaryAddr, timeout)
	if err != nil {
		return 0, fmt.Errorf("connect to primary: %w", err)
	}
	defer conn.Close()

	f := transport.NewConnFramer(conn)
	req := protocol.Request{Cmd: protocol.CmdPut, Key: []byte(key), Value: []byte(value), RequireQuorum: quorum}
	payload, err := protocol.EncodeRequest(req)
	if err != nil {
		return 0, fmt.Errorf("encode: %w", err)
	}

	if err := f.WriteWithTimeout(payload, timeout); err != nil {
		return 0, fmt.Errorf("write to primary: %w", err)
	}

	respPayload, err := f.ReadWithTimeout(timeout)
	if err != nil {
		return 0, fmt.Errorf("read from primary: %w", err)
	}

	resp, err := protocol.DecodeResponse(respPayload)
	if err != nil {
		return 0, fmt.Errorf("decode: %w", err)
	}

	switch resp.Status {
	case protocol.StatusOK:
		if quorum {
			fmt.Println("OK (quorum write succeeded)")
		} else {
			fmt.Println("OK")
		}
		return resp.Seq, nil // Return seq from primary
	case protocol.StatusError:
		fmt.Println("(server error)")
	default:
		fmt.Printf("(unexpected status: %d)\n", resp.Status)
	}
	return 0, nil
}

func doReplicaOf(f *transport.Framer, primaryAddr string, timeout time.Duration) error {
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

func doCleanup(f *transport.Framer, timeout time.Duration) error {
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
