package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/hpcloud/tail"
	"github.com/urfave/cli"
)

const version = "0.2.6"

var (
	username string
	url      string
	color    string
	attach   bool
	prefix   string
	syntax   string
)

func main() {
	app := cli.NewApp()
	app.Name = "mmlogmon"
	app.Usage = "Monitor logs and send updates to mattermost"
	app.Version = version
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "debug, d",
			Usage: "Enable debugging.",
		},
		cli.StringFlag{
			Name:  "prefix, p",
			Usage: "Prefix for messages.",
			Value: ":warning:",
		},
		cli.StringFlag{
			Name:  "syntax",
			Usage: "Syntax for logs.",
		},
		cli.StringFlag{
			Name:  "username, u",
			Usage: "Username for messaging mattermost",
		},
		cli.StringFlag{
			Name:  "url",
			Usage: "URL for mattermost webhook",
		},
		cli.StringFlag{
			Name:  "color",
			Usage: "Color for mattermost webhook",
			Value: "#FF0000",
		},
		cli.StringSliceFlag{
			Name:  "file, f",
			Usage: "Path to log file to watch",
		},
		cli.StringSliceFlag{
			Name:  "glob, g",
			Usage: "Glob path to log file to watch",
		},
		cli.BoolFlag{
			Name:  "reopen, F",
			Usage: "Reopen file if relocaed. (tail -F)",
		},
		cli.BoolFlag{
			Name:  "start-at-end, se",
			Usage: "Start tailling the file at the end",
		},
		cli.StringFlag{
			Name:  "begin, b",
			Usage: "Regex pattern to look for to indicate a new log message beginning. If omitted, each line is considered a unique message.",
		},
		cli.StringFlag{
			Name:  "end, e",
			Usage: "Regex pattern to look for to indicate a new log message beginning. If omitted, each line is considered a unique message.",
		},
		cli.StringSliceFlag{
			Name:  "exclude, x",
			Usage: "Regex pattern to exclude from notices.",
		},
		cli.IntFlag{
			Name:  "maxlines, max",
			Usage: "Maximum lines to be considered a unique log message. -1 indicates no limit",
			Value: -1,
		},
		cli.IntFlag{
			Name:  "minlines, min",
			Usage: "Minimum lines to be considered a unique log message. -1 indicates no limit. Use this in conjunction with the timeout and notifications will only be triggered when both the timeout is reached and minlines have been buffered.",
			Value: 1,
		},
		cli.UintFlag{
			Name:  "timeout, t",
			Usage: "Amount of time in milliseconds to wait before sending buffered lines. -1 disables the timer. 0 indicates no timer, and messages will be sent as soon a chunk of lines is processed.",
			Value: 1,
		},
		cli.BoolFlag{
			Name:  "no-attach",
			Usage: "Post logs as text instead of an attachment.",
		},
	}
	app.Action = Run
	err := app.Run(os.Args)
	if err != nil {
		panic(err)
	}
}

func Run(ctx *cli.Context) error {
	if ctx.Bool("debug") {
		log.SetLevel(log.DebugLevel)
		log.Info("Debug logging enabled")
	}
	username = ctx.String("username")
	url = ctx.String("url")
	color = ctx.String("color")
	prefix = ctx.String("prefix")
	attach = !ctx.Bool("no-attach")

	if len(ctx.StringSlice("file")) == 0 && len(ctx.StringSlice("glob")) == 0 {
		return fmt.Errorf("File or glob must be specified")
	}

	go mon(ctx)

	sig := make(chan os.Signal)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.Infof("Signal (%s) received, stopping\n", s)
	return nil
}

var (
	maxLines int
	minLines int
	timeout  uint
	b        *regexp.Regexp
	e        *regexp.Regexp
	x        []*regexp.Regexp
	c        tail.Config
)

func mon(ctx *cli.Context) {
	var (
		err error
	)
	maxLines = ctx.Int("maxlines")
	minLines = ctx.Int("minlines")
	if minLines < 0 {
		minLines = 1
	}
	timeout = ctx.Uint("timeout")
	if ctx.String("begin") != "" {
		b, err = regexp.Compile(ctx.String("begin"))
		if err != nil {
			log.WithError(err).WithField("begin", ctx.String("begin")).Error("Error compiling begin regex")
			return
		}
	}
	if ctx.String("end") != "" {
		e, err = regexp.Compile(ctx.String("end"))
		if err != nil {
			log.WithError(err).WithField("end", ctx.String("end")).Error("Error compiling end regex")
			return
		}
	}
	for _, ex := range ctx.StringSlice("exclude") {
		rx, err := regexp.Compile(ex)
		if err != nil {
			log.WithError(err).WithField("exclude", ctx.String("exclude")).Error("Error compiling exclude regex")
			return
		}
		x = append(x, rx)
	}
	c = tail.Config{
		Follow: true,
		Logger: log.StandardLogger(),
		Poll:   false,
	}
	if ctx.Bool("start-at-end") {
		c.Location = &tail.SeekInfo{Whence: 2}
	}
	c.ReOpen = ctx.Bool("reopen")

	files := ctx.StringSlice("file")
	for _, g := range ctx.StringSlice("glob") {
		m, err := filepath.Glob(g)
		if err != nil {
			log.WithError(err).WithField("glob", g).Error("Error parsing glob")
			continue
		}
		files = append(files, m...)
	}

	for _, file := range files {
		log.WithField("file", file).Debug("Starting watch on file")
		go monFile(file)
	}
}

func monFile(file string) {
	lb := make([]string, 0)
	t, err := tail.TailFile(file, c)
	if err != nil {
		log.WithError(err).WithField("file", file).Error("Error tailing file.")
		return
	}
	log.WithField("t", t).Debug("Tail setup")
MAIN:
	for {
		s := ""
		log.WithField("lb-len", len(lb)).Debug("Starting Loop")
		if len(lb) > 0 {
			log.WithField("s", s).Debug("String to compare")
			s = lb[len(lb)-1]
		}
		switch {
		case e != nil && e.MatchString(s):
			log.Debug("Matched end")
			go notify(lb, file)
			lb = nil
			continue MAIN
		case b != nil && len(lb) > 1 && b.MatchString(s):
			log.Debug("Matched begin")
			go notify(lb[:len(lb)-1], file)
			lb = nil
			lb = append(lb, s)
			continue MAIN
		case maxLines > 0 && len(lb) >= maxLines:
			log.Debug("Over max lines")
			go notify(lb, file)
			lb = nil
			continue MAIN
		case len(lb) < minLines:
			log.Debug("Under min lines, waiting for write")
			l := <-t.Lines
			if l != nil {
				log.Debug("Line written during minline wait")
				lb = append(lb, l.Text)
			}
			continue MAIN
		}

		// Keep grabbing lines that are avaliable, before giving the timer a cahnce
		log.Debug("Finished case statement, checking for new lines.")
		select {
		case l := <-t.Lines:
			log.Debug("New lines found in first check")
			lb = append(lb, l.Text)
			continue MAIN
		default:
		}

		log.Debug("Starting timer and waiting for new lines.")
		// Grab the next line, or print out when the timer expires
		to := time.NewTimer(time.Duration(timeout) * time.Millisecond)
		select {
		case l := <-t.Lines:
			log.Debug("New lines found during timer wait")
			lb = append(lb, l.Text)
			to.Stop()
			continue MAIN
		case _ = <-to.C:
			log.Debug("Timer expired, sending updates.")
			go notify(lb, file)
			lb = nil
			continue MAIN
		}
	}
}

func notify(lb []string, file string) {
	for _, ex := range x {
		for _, l := range lb {
			if ex.MatchString(l) {
				return
			}
		}
	}
	if b != nil && !b.MatchString(lb[0]) {
		return
	}
	if strings.TrimSpace(strings.Join(lb, "")) == "" {
		return
	}
	log.WithField("lb", lb).Debug("Notify triggered")
	p := make(map[string]interface{})
	p["username"] = username
	if attach {
		a := make(map[string]interface{})
		a["fallback"] = fmt.Sprintf("New log entry in %v. \n%v\n[...]%v\n", file, lb[0], lb[len(lb)-1])
		a["color"] = color
		a["pretext"] = fmt.Sprintf("%v New log entry in %v", prefix, file)
		// TODO: When MM PLT-3340 is fixed, wrap txt in code blocks
		txt := "  " + strings.Join(lb, "\n  ")
		a["text"] = txt
		p["attachments"] = []map[string]interface{}{a}
	} else {
		txt := strings.Join(lb, "\n")
		txt = fmt.Sprintf("%v New log entry in %v\n```%v\n%v\n```", prefix, file, syntax, txt)
		p["text"] = txt
	}
	pj, err := json.Marshal(p)
	if err != nil {
		log.WithError(err).WithField("payload", p).Error("Failed to marshall json")
		return
	}
	log.WithField("payload-json", string(pj)).Debug("Json prepared")
	r, err := http.Post(url, "application/json", bytes.NewBuffer(pj))
	if err != nil {
		if strings.Contains(err.Error(), "REFUSED_STREAM") {
			log.WithError(err).WithField("url", url).WithField("json", string(pj)).Debug("Failed to post json to url. Retrying.")
			time.Sleep(1 * time.Millisecond)
			go notify(lb, file)
			return
		}
		log.WithError(err).WithField("url", url).WithField("json", string(pj)).Error("Failed to post json to url.")
		return
	}
	log.WithField("Response", r).Debug("Response from web hook")
}
