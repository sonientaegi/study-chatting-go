package main

import (
	"container/list"
	"context"
	"fmt"
	"github.com/googollee/go-socket.io"
	"github.com/takama/daemon"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Configuration
const (
	SOCKCLIENT_EMIT_TIMEOUT = time.Second
	SOCKCLIENT_EMIT_BUFFER  = 10

	CHATROOM_EVENT_BUFFER = 10
	CHATROOM_CONN_BUFFER  = 1
	CHATROOM_DISC_BUFFER  = 1
	CHATROOM_HISTORY      = 20
)

// Notice
const (
	NOTICE_SHUTDOWN = "잠시 후 채팅서버를 종료합니다."
)

// Session
type Session struct {
	username string
}

// Authentication
type AuthRequest struct {
	Username string `json:"Username"`
}

type AuthResponse struct {
	IsAuthenticated bool   `json:"isAuthenticated"`
	Reason          string `json:"reason"`
}

//// ControlType
//const (
//	HISTORY = iota
//)
//
//var ControlType = [...]string{
//	"HISTORY",
//}
//
//type Control struct {
//	ControlType string 'json:"control_type"'
//}
//
//type ControlContainer struct {
//	sender string
//	control Control
//}

// EventType
const (
	SUBSCRIBE = iota
	UNSUBSCRIBE
	MESSAGE
)

var EventType = [...]string{
	"SUBSCRIBE",
	"UNSUBSCRIBE",
	"MESSAGE",
}

type Event struct {
	EventType string `json:"event_type"`
	Timestamp int    `json:"timestamp"` // Server processed time for outbound, otherwise 0.
	Username  string `json:"username"`  // Empty for inbound or notice outbound, otherwise sender's username.
	Message   string `json:"message"`   // Content for EventType[MESSAGE].
}

type EventContainer struct {
	sender string
	event  Event
}

func newEvent(eventType string, username string, message string) *Event {
	event := new(Event)
	event.EventType = eventType
	event.Timestamp = int(time.Now().Unix())
	event.Username = username
	event.Message = message

	return event
}

func newNoticeEvent(message string) *Event {
	return newEvent(EventType[MESSAGE], "--- 공지사항 ---", message)
}

// socket.io client
type SockClient struct {
	s               socketio.Conn
	isAuthenticated bool
	username        string

	events    chan Event
	signal    chan struct{}
	terminate chan struct{}
}

// subscribe create new SockClient instance.
func subscribe(s socketio.Conn) *SockClient {
	socketClient := new(SockClient)
	socketClient.s = s
	socketClient.isAuthenticated = true
	socketClient.username = s.Context().(*Session).username

	socketClient.signal = make(chan struct{})
	socketClient.events = make(chan Event, SOCKCLIENT_EMIT_BUFFER)
	socketClient.terminate = make(chan struct{})

	socketClient.run()

	return socketClient
}

// run launches looper of SockClient. Accents Event and emit to browser client.
// Looper terminates when channel signal has been closed.
func (sc *SockClient) run() {
	go func(sc *SockClient) {
		defer func() {
			if r := recover(); r != nil {
				log.Println(fmt.Sprintf("[%3s] %s Unexpected termination", sc.s.ID(), sc.username))
			}
		}()

		log.Println(fmt.Sprintf("[%3s] %s MAIN LOOP start", sc.s.ID(), sc.username))
	MainLoop:
		for {
			select {
			case <-sc.signal:
				break MainLoop
			case event := <-sc.events:
				sc.s.Emit("event", event)
			}
		}
		log.Println(fmt.Sprintf("[%3s] %s MAIN LOOP terminate", sc.s.ID(), sc.username))
		close(sc.terminate)
	}(sc)
}

// post asynchronous puts Event to channel.
func (sc *SockClient) post(pEvent *Event) {
	go sc.send(pEvent)
}

// send synchronous puts Event to channel.
func (sc *SockClient) send(pEvent *Event) bool {
	select {
	case sc.events <- *pEvent:
		return true
	case <-time.After(SOCKCLIENT_EMIT_TIMEOUT):
		log.Println(fmt.Sprintf("[%3s] %s Timeout", sc.s.ID(), sc.username))
		return false
	}
}

// unsubscribe request termination of looper and close all channels initiated in subscribe().
func (sc *SockClient) unsubscribe() {
	close(sc.signal)
	close(sc.events)

	select {
	case <-sc.terminate:
	}
}

func chatServer() {
	cwd, _ := os.Getwd()
	log.Println("CWD :", cwd)

	connection := make(chan socketio.Conn, CHATROOM_CONN_BUFFER)
	disconnection := make(chan socketio.Conn, CHATROOM_DISC_BUFFER)
	events := make(chan EventContainer, CHATROOM_EVENT_BUFFER)

	// socket.io 서버
	sockServer := socketio.NewServer(nil)
	sockServer.OnConnect("/", func(s socketio.Conn) error {
		log.Println(fmt.Sprintf("[%3s] connect", s.ID()))
		return nil
	})
	eventLog := list.New()

	sockServer.OnEvent("/", "authRequest", func(s socketio.Conn, authRequest AuthRequest) AuthResponse {
		log.Println(fmt.Sprintf("[%3s] Sign in", s.ID()))

		authResponse := AuthResponse{}
		if authRequest.Username == "admin" {
			authResponse.IsAuthenticated = false
			authResponse.Reason = "사용할 수 없는 사용자 이름입니다."

			log.Println(fmt.Sprintf("[%3s] %s is illeagal username", s.ID(), authRequest.Username))
			go s.Close()
		} else {
			authResponse.IsAuthenticated = true
			session := new(Session)
			session.username = authRequest.Username
			s.SetContext(session)

			log.Println(fmt.Sprintf("[%3s] %s is authenticated", s.ID(), authRequest.Username))
			connection <- s
		}
		return authResponse
	})

	sockServer.OnDisconnect("/", func(s socketio.Conn, reason string) {
		if s.Context() == nil {
			log.Println(fmt.Sprintf("[%3s] is not authenticated. Ignoring.", s.ID()))
		} else {
			log.Println(fmt.Sprintf("[%3s] %s disconnect", s.ID(), s.Context().(*Session).username))
			disconnection <- s
		}
	})

	sockServer.OnEvent("/", "event", func(s socketio.Conn, event Event) string {
		event.Username = s.Context().(*Session).username
		event.Timestamp = int(time.Now().Unix())
		events <- EventContainer{s.ID(), event} // struct{string Event} {s.ID(), event}

		return "200"
	})

	// Launch : socket.io Server.
	go sockServer.Serve()

	// Register SIGTERM
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, os.Kill, syscall.SIGTERM)

	// 웹서버
	var httpServer = &http.Server{
		Addr:    ":8000",
		Handler: nil,
	}

	// Launch : host looper.
	go func() {
		sockClients := list.New()
	MainLoop:
		for {
			select {
			case s := <-connection:
				newSockClient := subscribe(s)
				pEvent := newEvent(EventType[SUBSCRIBE], newSockClient.username, "")
				for iter := sockClients.Front(); iter != nil; iter = iter.Next() {
					sockClient := iter.Value.(*SockClient)
					sockClient.post(pEvent)
				}
				for iter := eventLog.Front(); iter != nil; iter = iter.Next() {
					event := iter.Value.(Event)
					newSockClient.post(&event)
					time.Sleep(time.Millisecond * 10)
				}
				sockClients.PushBack(newSockClient)
			case s := <-disconnection:
				pEvent := newEvent(EventType[UNSUBSCRIBE], s.Context().(*Session).username, "")
				for iter := sockClients.Front(); iter != nil; iter = iter.Next() {
					sockClient := iter.Value.(*SockClient)
					if sockClient.s == s {
						sockClients.Remove(iter)
						sockClient.unsubscribe()
					} else {
						sockClient.post(pEvent)
					}
				}
			case container := <-events:
				eventLog.PushBack(container.event)
				for {
					if eventLog.Len() <= CHATROOM_HISTORY {
						break
					} else {
						eventLog.Remove(eventLog.Front())
					}
				}

				for iter := sockClients.Front(); iter != nil; iter = iter.Next() {
					sockClient := iter.Value.(*SockClient)
					if sockClient.s.ID() == container.sender {
						continue
					} else {
						sockClient.post(&container.event)
					}
				}
			case killSignal := <-interrupt:
				log.Println("Server got signal :", killSignal)
				log.Println("Server shutting down")

				break MainLoop
			}
		}

		// Server is shutting down. Terminate all sockClients
		pEvent := newNoticeEvent(NOTICE_SHUTDOWN)

		wg := sync.WaitGroup{}
		for iter := sockClients.Front(); iter != nil; iter = iter.Next() {
			wg.Add(1)

			sockClient := iter.Value.(*SockClient)
			go func(sockClient *SockClient) {
				sockClient.send(pEvent)
				sockClient.unsubscribe()
				wg.Done()
			}(sockClient)
		}
		wg.Wait()
		sockServer.Close()
		httpServer.Shutdown(context.Background())

	}()

	http.Handle("/socket.io/", sockServer)
	http.Handle("/", http.FileServer(http.Dir("./static")))
	log.Println("Serving at localhost:8000.")

	log.Fatal(httpServer.ListenAndServe())
	log.Println("Server terminated.")
}

func manageService() bool {
	const usage = "Usage: study-chatting-go install | remove | start | stop | status"
	service, err := daemon.New("study-chatting-go", "Example", daemon.SystemDaemon)
	if err != nil {
		log.Println("Service : error -", err)
		os.Exit(1)
	}

	status, err := func() (string, error) {
		command := os.Args[1]

		switch command {
		case "install":
			return service.Install()
		case "remove":
			return service.Remove()
		case "start":
			return service.Start()
		case "stop":
			return service.Stop()
		case "status":
			return service.Status()
		default:
			return usage, nil
		}
	}()

	log.Println("Service :", status)
	if err != nil {
		log.Println("Service : error -", err)
		return false
	} else {
		return true
	}
}

func main() {
	if len(os.Args) > 1 {
		if manageService() == true {
			os.Exit(0)
		} else {
			os.Exit(2)
		}
	} else {
		chatServer()
	}
	os.Exit(0)
}
