package teldrive

import (
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/op"
)

type Addition struct {
	// Usually one of two
	driver.RootPath
	// define other
	Address           string `json:"url" required:"true" help:"Teldrive API host URL"`
	ChunkSize         int64  `json:"chunk_size" type:"number" default:"10" help:"Chunk size in MiB"`
	Cookie            string `json:"cookie" type:"string" required:"true" help:"access_token=xxx"`
	UploadConcurrency int64  `json:"upload_concurrency" type:"number" default:"4" help:"Concurrency upload requests"`
	ChannelID         int64  `json:"channel_id" type:"number" help:"Telegram channel ID"`
	EncryptFiles      bool   `json:"encrypt_files" type:"bool" default:"false" help:"Enable native teldrive encryption"`
	UploadHost        string `json:"upload_host" help:"Upload API host (optional)"`
}

var config = driver.Config{
	Name:        "Teldrive",
	DefaultRoot: "/",
	OnlyProxy:   true,
}

func init() {
	op.RegisterDriver(func() driver.Driver {
		return &Teldrive{}
	})
}
