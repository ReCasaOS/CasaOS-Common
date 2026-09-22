package external

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
)

const (
	MessageBusAddressFilename = "message-bus.url"
	APIMessageBus             = "/v2/message_bus"

	// MessageBusSocketFilename is the unix socket, in the runtime path, where
	// the message bus takes events.
	MessageBusSocketFilename = "message-bus.sock"
)

// MessageBusSocketPath is where the message bus listens for events over a unix
// socket: in the runtime path, which only root can write. It used to be
// /tmp/message-bus.sock, a name any local user could take first.
func MessageBusSocketPath(runtimePath string) string {
	return filepath.Join(runtimePath, MessageBusSocketFilename)
}

type EventType struct {
	Name             string
	SourceID         string
	PropertyTypeList []PropertyType
}

type PropertyType struct {
	Name        string
	Description *string
	Example     *string
}

func PrintEventTypesAsMarkdown(sourceID, version string, eventTypes []EventType) {
	fmt.Printf("## Source ID: `%s` (v%s)\n\n", sourceID, version)
	for _, eventType := range eventTypes {
		fmt.Printf("### Event Type: `%s`\n\n", eventType.Name)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)

		fmt.Fprintln(w, "| Property\t| Description\t| Example\t|")
		fmt.Fprintln(w, "| --------\t| -----------\t| -------\t|")

		for _, propertyType := range eventType.PropertyTypeList {
			fmt.Fprintf(w, "| `%s`\t|", propertyType.Name)

			if propertyType.Description != nil {
				fmt.Fprintf(w, " %s", *propertyType.Description)
			}

			fmt.Fprintf(w, "\t|")

			if propertyType.Example != nil {
				fmt.Fprintf(w, " `%s`", *propertyType.Example)
			}

			fmt.Fprintln(w, "\t|")
		}
		w.Flush()

		fmt.Println()
	}
}

func GetMessageBusAddress(runtimePath string) (string, error) {
	address, err := getAddress(filepath.Join(runtimePath, MessageBusAddressFilename))
	if err != nil {
		return "", err
	}

	return strings.TrimRight(address, "/") + APIMessageBus, nil
}

// PublishEventInSocket posts an event to the message bus through its unix
// socket in runtimePath. The response comes back with its body closed.
func PublishEventInSocket(ctx context.Context, runtimePath, sourceID, name string, properties map[string]string) (*http.Response, error) {
	socketPath := MessageBusSocketPath(runtimePath)
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
			// A transport per call: an idle connection kept alive would be leaked.
			DisableKeepAlives: true,
		},
	}

	body, err := json.Marshal(properties)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://unix/v2/message_bus/event/%s/%s", sourceID, name),
		bytes.NewBuffer(body),
	)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return resp, err
	}
	defer resp.Body.Close()
	return resp, nil
}
