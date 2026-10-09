// Copyright 2026 Sanoma AI
// SPDX-License-Identifier: Apache-2.0

// Command provider-bridge serves OpenTofu providers over ConnectRPC.
package main

import (
	"fmt"
	"os"

	_ "github.com/Sanoma-AI/provider-bridge/gen/bridge/v1/providerbridgev1connect"
	_ "github.com/Sanoma-AI/provider-bridge/gen/tfplugin5"
	_ "github.com/Sanoma-AI/provider-bridge/gen/tfplugin6"
)

func main() {
	fmt.Fprintln(os.Stderr, "provider-bridge: not implemented yet")
	os.Exit(2)
}
