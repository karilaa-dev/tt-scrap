// bench-api executes prepared requests in an isolated HTTP process. It never
// generates identities, signs requests, or accesses the identity database.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

type command struct {
	ID          string            `json:"id"`
	Operation   string            `json:"operation"`
	Proxy       *string           `json:"proxy"`
	Concurrency int               `json:"concurrency"`
	TimeoutMS   int               `json:"requestTimeoutMs"`
	Requests    []preparedRequest `json:"requests"`
	invalid     bool
}
type reply struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  any    `json:"error,omitempty"`
}

func run(ctx context.Context, input io.Reader, output io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	commands := make(chan command, 1)
	go func() {
		defer close(commands)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64<<10), 4<<20)
		for scanner.Scan() {
			var cmd command
			if json.Unmarshal(scanner.Bytes(), &cmd) != nil || cmd.ID == "" || cmd.Operation == "" {
				cmd = command{ID: "-", invalid: true}
			}
			select {
			case commands <- cmd:
			case <-ctx.Done():
				return
			}
		}
		if scanner.Err() != nil {
			select {
			case commands <- command{ID: "-", invalid: true}:
			case <-ctx.Done():
			}
		}
	}()
	var outputMu sync.Mutex
	encoder := json.NewEncoder(output)
	send := func(id string, result any, err error) {
		message := reply{ID: id, OK: err == nil, Result: result}
		if err != nil {
			message.Result = nil
			message.Error = map[string]string{"code": "http_executor_failed", "message": "HTTP benchmark executor failed."}
		}
		outputMu.Lock()
		err = encoder.Encode(message)
		outputMu.Unlock()
		if err != nil {
			cancel()
		}
	}
	var executor *httpExecutor
	var tasks sync.WaitGroup
	var jobMu sync.Mutex
	var jobCancel context.CancelFunc
	cancelJob := func() {
		jobMu.Lock()
		defer jobMu.Unlock()
		if jobCancel != nil {
			jobCancel()
		}
	}
	defer func() {
		cancelJob()
		if closer, ok := input.(io.Closer); ok {
			closer.Close()
		}
		if executor != nil {
			executor.close()
		}
		tasks.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case cmd, ok := <-commands:
			if !ok {
				return nil
			}
			if cmd.invalid {
				send(cmd.ID, nil, errors.New("invalid command"))
				continue
			}
			switch cmd.Operation {
			case "init":
				if executor != nil {
					send(cmd.ID, nil, errors.New("already initialized"))
					continue
				}
				capacity, timeout := cmd.Concurrency, cmd.TimeoutMS
				if capacity == 0 {
					capacity = 8
				}
				if timeout == 0 {
					timeout = 30000
				}
				created, err := newExecutor(cmd.Proxy, capacity, timeout)
				if err != nil {
					send(cmd.ID, nil, err)
					continue
				}
				executor = created
				send(cmd.ID, map[string]int{"pid": os.Getpid()}, nil)
			case "execute":
				if executor == nil {
					send(cmd.ID, nil, errors.New("not initialized"))
					continue
				}
				jobMu.Lock()
				if jobCancel != nil {
					jobMu.Unlock()
					send(cmd.ID, nil, errors.New("executor busy"))
					continue
				}
				jobCtx, stopJob := context.WithCancel(ctx)
				jobCancel = stopJob
				jobMu.Unlock()
				tasks.Go(func() {
					result, err := executor.execute(jobCtx, cmd.Requests, cmd.Concurrency)
					jobMu.Lock()
					stopJob()
					jobCancel = nil
					jobMu.Unlock()
					send(cmd.ID, result, err)
				})
			case "cancel":
				cancelJob()
				if executor != nil {
					executor.cancel()
				}
				tasks.Wait()
				send(cmd.ID, map[string]bool{"cancelled": true}, nil)
			case "shutdown":
				cancelJob()
				if executor != nil {
					executor.close()
				}
				tasks.Wait()
				send(cmd.ID, map[string]bool{"closed": true}, nil)
				return nil
			default:
				send(cmd.ID, nil, errors.New("unknown operation"))
			}
		}
	}
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		os.Exit(1)
	}
}
