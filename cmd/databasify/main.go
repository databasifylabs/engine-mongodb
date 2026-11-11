package main

import (
	"os"

	mongodb "github.com/databasifylabs/engine-mongodb/engine"
	sdk "github.com/databasifylabs/engine-sdk"
)

func main() {
	os.Exit(sdk.Run(mongodb.New()))
}
