package main

import (
	"flag"
	"github.com/azurity/xmodem-go"
	"io"
	"io/fs"
	"log"
	"os"
	"strings"
	"time"
)

func main() {
	recv := flag.Bool("r", false, "recv")
	send := flag.Bool("s", false, "send")
	xMode := flag.Bool("x", false, "xmodem")
	yMode := flag.Bool("y", false, "xmodem")
	use1K := flag.Bool("k", false, "1k-block")
	useCRC := flag.Bool("c", false, "crc")
	useCAN := flag.Bool("d", false, "double can")
	useG := flag.Bool("g", false, "g option")

	flag.Parse()

	files := flag.Args()

	if *send && len(files) == 0 {
		log.Panicln("need at least one file")
	}

	if *recv == *send {
		log.Panicln("must recv or send")
	}
	if *xMode == *yMode {
		log.Panicln("must xmodem or ymodem")
	}

	var fn xmodem.ModemFn = 0
	if *useCRC {
		fn = fn | xmodem.ModemFnCRC
	}
	if *use1K {
		fn = fn | xmodem.ModemFn1k
	}
	if *useCAN {
		fn = fn | xmodem.ModemFnCANCAN
	}
	if *useG {
		fn = fn | xmodem.ModemFnG
	}

	var conf xmodem.ModemConfig

	if *xMode {
		conf = xmodem.XModemConfig(fn)
		if *send && len(files) > 1 {
			log.Panicln("xmodem do not support multi file")
		}
		if *recv && len(files) == 0 {
			log.Panicln("xmodem need save file name")
		}
	}
	if *yMode {
		conf = xmodem.YModemConfig(fn)
	}

	m, r, _ := xmodem.NewModem(conf, os.Stdin, os.Stdout)
	go io.ReadAll(r)

	if *recv {
		err := m.Receive(func(file xmodem.File) {
			if *xMode {
				f, err := os.OpenFile(files[0], os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.ModePerm)
				if err != nil {
					log.Panicln(err)
				}
				defer f.Close()
				io.Copy(f, file.Body)
			} else {
				if file.Path == "" {
					log.Panicln("recv blank path")
				}
				p := strings.Replace(file.Path, "/", "_", -1)
				f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.ModePerm)
				if err != nil {
					log.Panicln(err)
				}
				io.Copy(f, file.Body)
				f.Close()
				os.Chmod(p, file.Mode)
				os.Chtimes(p, time.Now(), file.ModTime)
			}
		})
		if err != nil {
			log.Panicln(err)
		}
	}
	if *send {
		sFiles := []xmodem.File{}
		for _, file := range files {
			f, err := os.Open(file)
			if err != nil {
				log.Panicln(err)
			}
			defer f.Close()
			stat, err := f.Stat()
			if err != nil {
				log.Panicln(err)
			}
			sFiles = append(sFiles, xmodem.File{
				Path:    file,
				Length:  stat.Size(),
				ModTime: stat.ModTime(),
				Mode:    stat.Mode(),
				Body:    f,
			})
		}
		if *xMode {
			err := m.SendBytes(sFiles[0].Body)
			if err != nil {
				log.Panicln(err)
			}
		} else {
			err := m.SendList(sFiles)
			if err != nil {
				log.Panicln(err)
			}
		}
	}
}
