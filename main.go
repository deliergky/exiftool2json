package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

type Description struct {
	Language string `xml:"lang,attr"`
	Content  string `xml:",chardata"`
}

type Tag struct {
	Writable       bool              `json:"writable" xml:"writable,attr"`
	Path           string            `json:"path" xml:"name,attr"`
	Group          string            `json:"group"`
	Descriptions   []Description     `xml:"desc" json:"-"`
	DescriptionMap map[string]string `json:"descriptions"`
	Type           string            `json:"type" xml:"type,attr"`
}

func (t Tag) CreateDescriptionMap() {
	for _, description := range t.Descriptions {
		t.DescriptionMap[description.Language] = description.Content
	}
}

// getXMLAttribute returns the value of the first attribute with the given name.
func getXMLAttribute(atts []xml.Attr, name string) *string {
	for _, a := range atts {
		if a.Name.Local == name {
			return &a.Value
		}
	}
	return nil
}

func closeReader(rc io.ReadCloser) {
	err := rc.Close()
	if err != nil {
		log.Printf("Error closing reader: %v\n", err)
	}
}

func handle(ctx context.Context, cancelFunc context.CancelFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			closeReader(r.Body)
		}()

		var eof bool
		var includeSeparator bool
		var tableName *string

		w.Header().Add("Content-Type", "application/json")
		cmd := exec.CommandContext(ctx, "exiftool", "-listx")
		reader, err := cmd.StdoutPipe()

		defer func() {
			closeReader(reader)
		}()

		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			log.Printf("Error piping content: %v\n", err)
			return
		}
		err = cmd.Start()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			log.Printf("Error starting: %v\n", err)
			return
		}
		decoder := xml.NewDecoder(reader)
		_, err = io.WriteString(w, "{\"tags\":[\n")

		for !eof {
			token, err := decoder.Token()
			if err != nil {
				if err != io.EOF {
					cancelFunc()
					log.Printf("%v\n", err)
					return
				}
				_, err := w.Write(nil)
				if err != nil {
					log.Printf("%v\n", err)
				}
				eof = true
			}

			switch n := token.(type) {
			case xml.StartElement:
				switch n.Name.Local {
				case "table":
					tableName = getXMLAttribute(n.Attr, "name")
				case "tag":
					tag := Tag{DescriptionMap: make(map[string]string)}
					err = decoder.DecodeElement(&tag, &n)
					if err != nil {
						log.Printf("Error decoding: %v\n", err)
					}
					if tableName != nil {
						tag.Group = *tableName
						tag.Path = fmt.Sprintf("%s:%s", tag.Group, tag.Path)
					}
					if includeSeparator {
						_, err = io.WriteString(w, ",")
					}
					includeSeparator = true
					tag.CreateDescriptionMap()

					err = json.NewEncoder(w).Encode(tag)
					if err != nil {
						cancelFunc()
						log.Printf("Error writing: %v\n", err)
					}
				}
			default:
			}
		}
		_, err = io.WriteString(w, "]}\n")
	}
}

// exiftool needs to be installed prior running
// port 8080 needs to be free prior running

// run with go run main.go
func main() {
	ctx := context.Background()
	ctx, cancelCommand := context.WithCancel(ctx)

	shutdown := make(chan os.Signal, 1)
	serviceErrors := make(chan error, 1)

	http.HandleFunc("/tags", handle(ctx, cancelCommand))

	server := http.Server{
		Addr: ":8080",
	}

	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	go func() {
		log.Println("Starting serving requests")
		serviceErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serviceErrors:
		cancelCommand()
		log.Printf("Error when serving requests %v", err)
		os.Exit(1)
	case sig := <-shutdown:
		log.Println("Received interrupt, shutting down server gracefully")
		cancelCommand()
		serverContext, cancelServer := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelServer()
		err := server.Shutdown(serverContext)
		if err != nil {
			log.Printf("Error shutting down web server %v", err)
			err = server.Close()
		}

		switch {
		case sig == syscall.SIGSTOP:
			log.Printf("Unexepcted server interrupt %+v %v", sig, ctx)
			os.Exit(1)
		case err != nil:
			log.Printf("Error shutting down server %v", err)
			os.Exit(1)
		default:
			log.Println("Server shutdown completed successfully")
			os.Exit(0)
		}
	}
}
