package main

import (
	"github.com/googollee/go-socket.io"
	"log"
	"net/http"
)

func main() {
	server := socketio.NewServer(nil)
	server.OnConnect("/", func(s socketio.Conn) error {
		s.SetContext("")
		log.Println("connected:", s.ID())
		return nil
	})

	http.Handle("/socket.io/", server)
	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.ListenAndServe(":80", nil)
}
