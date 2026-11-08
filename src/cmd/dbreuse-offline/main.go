package main

import (
	"os"

	"github.com/aldinokemal/go-whatsapp-web-multidevice/infrastructure/dbreuse"
)

func main() {
	os.Exit(dbreuse.RunOffline(os.Args[1:]))
}
