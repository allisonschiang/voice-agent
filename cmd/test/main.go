// cmd/test — minimal smoke test for a deployed text-router.
// Sends a single {"test": "<phrase>"} DoCommand and prints the response.
//
// Usage:
//
//	go run ./cmd/test \
//	  -addr=<machine-address>.viam.cloud \
//	  -api-key-id=<id> \
//	  -api-key=<secret> \
//	  -resource=voice-router \
//	  -phrase="wipe"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/robot/client"
	generic "go.viam.com/rdk/services/generic"
	"go.viam.com/utils/rpc"
)

func main() {
	addr := flag.String("addr", "", "machine address (e.g. my-machine-main.1a2b3c.viam.cloud)")
	apiKeyID := flag.String("api-key-id", "", "Viam API key id")
	apiKey := flag.String("api-key", "", "Viam API key")
	resource := flag.String("resource", "voice-router", "name of the router resource on the machine")
	phrase := flag.String("phrase", "wipe", "phrase to dry-run against the router")
	dispatch := flag.Bool("dispatch", false, "if true, actually dispatch (via 'input'); otherwise dry-run via 'test'")
	flag.Parse()

	if *addr == "" || *apiKeyID == "" || *apiKey == "" {
		fmt.Fprintln(os.Stderr, "addr, api-key-id, and api-key are required")
		flag.Usage()
		os.Exit(2)
	}

	ctx := context.Background()
	logger := logging.NewLogger("router-test")

	robot, err := client.New(ctx, *addr, logger, client.WithDialOptions(
		rpc.WithEntityCredentials(*apiKeyID, rpc.Credentials{
			Type:    rpc.CredentialsTypeAPIKey,
			Payload: *apiKey,
		}),
	))
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}
	defer robot.Close(ctx)

	router, err := generic.FromRobot(robot, *resource)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resource %q not found: %v\n", *resource, err)
		os.Exit(1)
	}

	key := "test"
	if *dispatch {
		key = "input"
	}
	resp, err := router.DoCommand(ctx, map[string]interface{}{key: *phrase})
	if err != nil {
		fmt.Fprintf(os.Stderr, "DoCommand failed: %v\n", err)
		os.Exit(1)
	}

	out, _ := json.MarshalIndent(resp, "", "  ")
	fmt.Printf("phrase=%q (%s)\nresponse:\n%s\n", *phrase, key, out)
}
