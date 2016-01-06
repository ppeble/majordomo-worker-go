package majordomo_worker

import (
	"errors"
	"fmt"
	"time"

	"git.sittercity.com/core-services/majordomo-worker-go.git/Godeps/_workspace/src/github.com/pebbe/zmq4"
)

type mdWorker struct {
	shutdown chan bool

	brokerAddress string
	serviceName   string

	heartbeat        time.Duration
	reconnect        time.Duration
	pollInterval     time.Duration
	maxLivenessCount int
	liveness         int
	heartbeatAt      time.Time

	socket  *zmq4.Socket
	context *zmq4.Context

	workerAction WorkerAction
	logger       Logger
}

func newWorker(context *zmq4.Context, brokerAddress, serviceName string, heartbeatInMillis, reconnectInMillis, pollInterval time.Duration, maxLivenessCount int, action WorkerAction, logger Logger) *mdWorker {
	w := &mdWorker{
		brokerAddress:    brokerAddress,
		serviceName:      serviceName,
		heartbeat:        heartbeatInMillis,
		reconnect:        reconnectInMillis,
		pollInterval:     pollInterval,
		context:          context,
		maxLivenessCount: maxLivenessCount,
		liveness:         0,
		workerAction:     action,
		shutdown:         make(chan bool),
		logger:           logger,
	}

	w.reconnectToBroker()
	return w
}

func (w *mdWorker) Receive() (msg [][]byte, err error) {
	for {
		select {
		case <-w.shutdown:
			w.cleanup()
			return msg, errors.New("Graceful shutdown")
		default:
			poll := zmq4.NewPoller()
			poll.Add(w.socket, zmq4.POLLIN)

			var polled []zmq4.Polled
			polled, err = poll.Poll(w.pollInterval)

			if err != nil {
				logError(w.logger, fmt.Sprintf("Polling socket failed, error: %s", err.Error()))
				continue
			}

			if len(polled) > 0 {
				msg, _ = w.socket.RecvMessageBytes(0)

				if len(msg) < 3 {
					logError(w.logger, fmt.Sprintf("Received invalid message (not enough frames), received %d", len(msg)))
					continue // ignore invalid messages
				}

				w.liveness = w.maxLivenessCount

				switch command := string(msg[2]); command {
				case MD_REQUEST:
					logDebug(w.logger, fmt.Sprintf("Received MD_REQUEST from broker with message '%q'", msg[5:]))
					replyTo := msg[3]

					actionResponse := w.workerAction.Call(msg[5:])
					reply := [][]byte{nil}
					reply = append(reply, actionResponse...)

					w.sendToBroker(MD_REPLY, replyTo, reply)

					msg = actionResponse
					return
				case MD_DISCONNECT:
					logDebug(w.logger, "Received MD_DISCONNECT from broker")
					w.reconnectToBroker() // Initiate a reconnect, which basically resets the connection
				case MD_HEARTBEAT:
					// Do nothing, ANY message coming in acts as a heartbeat so we handle it above
					logDebug(w.logger, "Received MD_HEARTBEAT from broker")
				default:
					// Do nothing, if we received something we don't recognize we'll just ignore it
					logDebug(w.logger, fmt.Sprintf("Received unknown command of %s'", msg[2]))
				}
			} else if w.liveness--; w.liveness <= 0 {
				logWarn(w.logger, fmt.Sprintf("Worker has received nothing from the broker for %d polls, sleeping for %s and reconnecting", w.maxLivenessCount, w.reconnect))
				time.Sleep(w.reconnect)
				w.reconnectToBroker()
			}

			if w.heartbeatAt.Before(time.Now()) {
				w.sendToBroker(MD_HEARTBEAT, nil, nil)
				w.heartbeatAt = time.Now().Add(w.heartbeat)
			}
		}
	}
}

func (w *mdWorker) Shutdown() {
	logDebug(w.logger, "Worker attempting graceful shutdown...")
	w.shutdown <- true
}

func (w *mdWorker) reconnectToBroker() (err error) {
	if w.socket != nil {
		w.socket.Close()
	}

	logDebug(w.logger, fmt.Sprintf("Attempting connection to broker at '%s'", w.brokerAddress))
	w.socket, _ = w.context.NewSocket(zmq4.DEALER)
	w.socket.SetLinger(0)
	w.socket.Connect(w.brokerAddress)
	logDebug(w.logger, fmt.Sprintf("Connected successfully to broker at '%s'", w.brokerAddress))

	w.sendToBroker(MD_READY, []byte(w.serviceName), nil)

	w.liveness = w.maxLivenessCount
	w.heartbeatAt = time.Now().Add(w.heartbeat)

	return
}

func (w *mdWorker) sendToBroker(command string, serviceName []byte, msg [][]byte) error {
	workerMessage := [][]byte{[]byte(""), []byte(MD_WORKER), []byte(command)}

	if serviceName != nil {
		workerMessage = append(workerMessage, serviceName)
	}

	if msg != nil {
		workerMessage = append(workerMessage, msg...)
	}

	_, err := w.socket.SendMessage(workerMessage)

	logDebug(w.logger, fmt.Sprintf("Sent command '%s' to broker with message '%q'", command, msg))

	return err
}

func (w *mdWorker) cleanup() {
	if w.socket != nil {
		w.socket.Close()
	}
	w.context.Term()
	logDebug(w.logger, "Worker socket and context closed successfully")
}
