package main

import (
	"fmt"
	"log"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "website ok")
	})
	log.Println("website listening on 4002")
	log.Fatal(http.ListenAndServe("127.0.0.1:4002", nil))
}
