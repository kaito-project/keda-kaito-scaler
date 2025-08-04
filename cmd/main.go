// Copyright (c) Microsoft Corporation.
// Licensed under the MIT license.

package main

import (
	"os"

	"k8s.io/component-base/cli"

	"github.com/kaito-project/keda-kaito-scaler/cmd/app"
)

func main() {
	command := app.NewKedaKaitoScalerCommand()
	code := cli.Run(command)
	os.Exit(code)
}
