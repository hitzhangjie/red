package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gdamore/tcell"
	"github.com/rivo/tview"
	"github.com/satyrius/gonx"

	"github.com/antonmedv/red/internal/prettyjson"
)

var (
	// options
	duration    time.Duration
	distance    int
	format      string
	nginxConfig string
	nginxFormat string
	showHelp    bool

	// args
	keys []string

	app   *tview.Application
	table *tview.Table
	store *Store
)

const (
	trendColumn int = iota
	countColumn
	firstDataColumn
)

const (
	helpMsg = `
"red" is an utility to visualize log events from reading or tailing log files,
this repo is forked from https://github.com/hokaccha/red, which inspires me
to improve "red" to support more formats, including zaplog.

red support 2 formats:
- json, 
  {"datetime": "2024-08-22 09:00:06.956", "level": "ERROR", "pos": "dbsvr/counter.go:202" "func": "[GetCounterBatch]", "msg": "empty counter list", "process": 8982, "traceID": 16029078675928157035, "meta.PlayerID": 0}
- zaplog,
  2024-08-22 09:00:06.956 ERROR dbsvr/counter.go:202 [GetCounterBatch] empty counter list {"process": 8982, "traceID": 16029078675928157035, "meta.PlayerID": 0}`
)

func init() {
	flag.DurationVar(&duration, "trend", 10*time.Second, "duration of trend")
	flag.IntVar(&distance, "distance", 3, "levenshtein distance for combining similar log entities")

	// red support 2 formats:
	// - json: {"datetime": "2024-08-22 09:00:06.956", "level": "ERROR", "pos": "dbsvr/counter.go:202" "func": "[GetCounterBatch]", "msg": "empty counter list", "process": 8982, "traceID": 16029078675928157035, "meta.PlayerID": 0}
	// - zaplog: 2024-08-22 09:00:06.956 ERROR dbsvr/counter.go:202 [GetCounterBatch] empty counter list {"process": 8982, "traceID": 16029078675928157035, "meta.PlayerID": 0}
	flag.StringVar(&format, "format", "zaplog", "stdin format, json or zaplog")

	// don't need this
	flag.StringVar(&nginxConfig, "nginx-config", "/etc/nginx/nginx.conf", "nginx config file")
	flag.StringVar(&nginxFormat, "nginx-format", "main", "nginx log_format name")

	flag.BoolVar(&showHelp, "help", false, "show help")
}

func main() {
	flag.Parse()
	keys = flag.Args()

	if showHelp {
		fmt.Println(helpMsg)
		fmt.Println()
		flag.Usage()
		os.Exit(2)
	}

	fout, err := os.OpenFile("red.log", os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		panic(err)
	}
	defer fout.Close()
	log.SetOutput(fout)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		<-ch
		fout.Close()
		os.Exit(0)
	}()

	store = NewStore(duration, distance, keys)
	app = tview.NewApplication()

	viewerOpen := false
	viewer := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)
	viewer.SetBorder(true)

	table = tview.NewTable().
		SetFixed(1, 2).
		SetDoneFunc(func(key tcell.Key) {
			if key == tcell.KeyEscape {
				table.SetSelectable(false, false)
			}
			if key == tcell.KeyEnter {
				table.SetSelectable(true, false)
			}
		})
	renderColumns()

	flex := tview.NewFlex()
	flex.AddItem(table, 0, 1, true)
	app.SetRoot(flex, true)

	showRowData := func() {
		store.RLock()
		row, _ := table.GetSelection()
		if row == 0 {
			row = 1
		}
		data := store.Get(row - 1).GetData()
		store.RUnlock()

		text, err := prettyjson.Marshal(data)
		if err != nil {
			panic(err)
		}
		log.Println("data after jsonmarshal", string(text))

		viewer.SetText(tview.TranslateANSI(string(text)))
		viewer.ScrollToBeginning()
	}

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyDown || event.Key() == tcell.KeyUp {
			table.SetSelectable(true, false)
			if viewerOpen {
				showRowData()
			}
		}
		if event.Key() == tcell.KeyEnter && !viewerOpen {
			viewerOpen = true
			flex.AddItem(viewer, 0, 1, false)
			showRowData()
		}
		if event.Key() == tcell.KeyEsc && viewerOpen {
			viewerOpen = false
			flex.RemoveItem(viewer)
		}
		return event
	})

	switch format {
	case "json", "zaplog":
		go read()
	case "nginx":
		go readNginx()
	}

	go draw()
	go shift(duration)

	if err := app.Run(); err != nil {
		panic(err)
	}
}

func renderColumns() {
	headerCell := func(s string) *tview.TableCell {
		return tview.NewTableCell(s).
			SetBackgroundColor(tcell.ColorRed).
			SetTextColor(tcell.ColorBlack).
			SetAlign(tview.AlignCenter).
			SetSelectable(false)
	}

	table.SetCell(0, trendColumn, headerCell("trend"))
	table.SetCell(0, countColumn, headerCell("count"))
	for i, key := range keys {
		table.SetCell(0, firstDataColumn+i, headerCell(key))
	}
}

func update(value map[string]interface{}) {
	if len(keys) == 0 {
		keys = mapKeys(value)
		store.SetKeys(keys)
		renderColumns()
	}

	store.Lock()
	store.Push(value)
	store.Unlock()
}

func read() {
	var dec Decoder
	switch format {
	case "json":
		dec = newJsonDecoder(os.Stdin)
	case "zaplog":
		dec = newZaplogDecoder(os.Stdin)
	}

	for dec.More() {
		value, err := dec.Decode()
		if err != nil {
			if err != io.EOF {
				log.Println(err)
				app.Stop()
			}
		}

		update(value)
	}
}

func readNginx() {
	config, err := os.Open(nginxConfig)
	if err != nil {
		panic(err)
	}
	defer config.Close()

	reader, err := gonx.NewNginxReader(os.Stdin, config, nginxFormat)
	if err != nil {
		panic(err)
	}
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			panic(err)
		}
		// Process the record... e.g.
		fmt.Printf("Parsed entry: %+v\n", rec)
	}
}

func readCommon(format string) {
	reader := gonx.NewReader(os.Stdin, format)
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			panic(err)
		}
		// Process the record... e.g.
		fmt.Printf("Parsed entry: %+v\n", rec)
	}
}

func shift(duration time.Duration) {
	for {
		store.Lock()
		store.Shift()
		store.Unlock()
		time.Sleep(duration / trendSize)
	}
}

func draw() {
	for {
		app.QueueUpdateDraw(func() {
			store.RLock()
			defer store.RUnlock()

			row := 1
			for ; row < table.GetRowCount(); row++ {
				data := store.Get(row - 1)
				table.GetCell(row, trendColumn).SetText(Spark(data.GetTrend()))
				table.GetCell(row, countColumn).SetText(data.GetCount())
				for j := 0; j < len(keys); j++ {
					text := fmt.Sprintf("%v", data.Get(keys[j]))
					table.GetCell(row, firstDataColumn+j).SetText(text)
				}
			}

			for ; row <= store.Len(); row++ {
				data := store.Get(row - 1)
				table.SetCell(row, trendColumn, tview.NewTableCell(Spark(data.GetTrend())).
					SetSelectable(false))
				table.SetCell(row, countColumn, tview.NewTableCell(data.GetCount()).
					SetSelectable(false))
				for j := 0; j < len(keys); j++ {
					text := fmt.Sprintf("%v", data.Get(keys[j]))
					table.SetCellSimple(row, firstDataColumn+j, text)
				}
			}
		})
		time.Sleep(100 * time.Millisecond)
	}
}
