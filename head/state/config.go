package state

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"github.com/hashicorp/hcl"
	"github.com/temoto/vender/hardware/mdb"
	"github.com/temoto/vender/helpers"
)

type Config struct {
	Mdb struct {
		uart_driver  string
		Debug        bool
		Uarter       mdb.Uarter
		UartDevice   string `hcl:"uart_device"`
		UartBaudrate int    `hcl:"uart_baudrate"`
	}
	Papa struct {
		Address  string
		CertFile string
	}
}

// Context["config"] -> *Config or panic
func GetConfig(ctx context.Context) *Config {
	v := ctx.Value("config")
	if v == nil {
		panic("context['config'] is nil")
	}
	if cfg, ok := v.(*Config); ok {
		return cfg
	}
	panic("context['config'] expected type *Config")
}

func ReadConfig(r io.Reader) (*Config, error) {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}
	c := new(Config)
	err = hcl.Unmarshal(b, c)

	if c.Mdb.UartBaudrate == 0 {
		c.Mdb.UartBaudrate = 9600
	}
	switch c.Mdb.uart_driver {
	case "", "file":
		c.Mdb.Uarter = mdb.NewFileUart()
	case "fast":
		c.Mdb.Uarter = mdb.NewFastUart()
	default:
		return nil, fmt.Errorf("config: unknown mdb.uart_driver=%s valid: file, fast", c.Mdb.uart_driver)
	}

	return c, err
}

func ReadConfigFile(path string) (*Config, error) {
	if pathAbs, err := filepath.Abs(path); err != nil {
		log.Printf("filepath.Abs(%s) error: %s", path, err)
	} else {
		path = pathAbs
	}
	log.Printf("reading config file %s", path)

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadConfig(f)
}

func MustReadConfig(fatal helpers.FatalFunc, r io.Reader) *Config {
	c, err := ReadConfig(r)
	if err != nil {
		fatal(err)
	}
	return c
}

func MustReadConfigFile(fatal helpers.FatalFunc, path string) *Config {
	c, err := ReadConfigFile(path)
	if err != nil {
		fatal(err)
	}
	return c
}