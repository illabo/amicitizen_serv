package main

import (
	"compress/bzip2"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/boltdb/bolt"
	"golang.org/x/crypto/ssh/terminal"
)

type Config struct {
	RemoteFile  string
	Port        int
	UpdateEvery int
}

type Status int

const (
	ready Status = iota
	processing
	failed
)

type CtxKey int

const (
	lastStatus CtxKey = iota
	syncedStatus
	updateRequired
	dbInstance
)

func main() {
	var cfgPath = flag.String("config", "config.toml", "custom path to config file")
	var cfg Config
	_, err := toml.DecodeFile(*cfgPath, &cfg)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	db, err := bolt.Open("db/data.db", 0644, nil)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer db.Close()

	var lastStatus = make(chan Status)
	var syncedStatus = make(chan Status)
	var updateRequired = make(chan bool)

	go syncStatus(lastStatus, syncedStatus)
	go updateManager(cfg.RemoteFile, db, lastStatus, updateRequired)
	go updateScheduller(updateRequired, cfg.UpdateEvery)
	go syncHelper(syncedStatus)

	mux := http.NewServeMux()
	mux.HandleFunc("/", setDBContext(handlePassportValid, db))
	mux.HandleFunc("/update", setChannelsContext(kickstartUpdate, lastStatus, syncedStatus, updateRequired))
	fmt.Println(http.ListenAndServe(fmt.Sprintf(":%d", cfg.Port), mux))
}

func handlePassportValid(res http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(res, "Only POST accepted", http.StatusBadRequest)
		return
	}
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		fmt.Println(err)
		http.Error(res, err.Error(), http.StatusBadRequest)
		return
	}
	bodyStr := string(body)
	if len(bodyStr) < 9 {
		http.Error(res, "Passport number too short", http.StatusUnprocessableEntity)
		return
	}
	fmt.Println(bodyStr)
	db := req.Context().Value(dbInstance).(*bolt.DB)
	blacklisted, err := isBlacklisted(db, bodyStr)
	if err != nil {
		fmt.Println(err)
		http.Error(res, err.Error(), http.StatusInternalServerError)
		return
	}
	switch blacklisted {
	case false:
		res.Write([]byte("valid"))
		return
	case true:
		res.Write([]byte("invalid"))
		return
	}
}

func isBlacklisted(db *bolt.DB, num string) (bool, error) {
	fmt.Println("checking if blacklisted: ", num)
	blacklisted := false

	fmt.Println(num)

	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("passports"))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(strings.TrimSpace(num)))
		if string(v) == "" {
			fmt.Println("Not found")
			return nil
		}
		fmt.Println("Found")
		blacklisted = true
		return nil
	})

	return blacklisted, err
}

func kickstartUpdate(res http.ResponseWriter, req *http.Request) {
	syncedStatus := req.Context().Value(syncedStatus).(chan Status)
	status := <-syncedStatus
	fmt.Println("Kickstarter got status:", status)
	syncedStatus <- status
	switch status {
	case ready, failed:
		fmt.Println("Apt to start update")
		updateRequiredChan := req.Context().Value(updateRequired).(chan bool)
		go func(updateRequiredChan chan bool) { updateRequiredChan <- true }(updateRequiredChan)
	case processing:
		fmt.Println("Update is already in progress")
		res.Write([]byte("Update is already in progress"))
		return
	}
	res.Write([]byte("Checking remote for updated dataset"))
}

func syncStatus(lastStatus, syncedStatus chan Status) {
	var currentStatus = ready
	for {
		l := <-lastStatus
		var stat string
		switch l {
		case 0:
			stat = "ready"
		case 1:
			stat = "in progress"
		case 2:
			stat = "failed"
		default:
			stat = "unknown"
		}
		fmt.Println("Got status update, current status: ", stat)
		currentStatus = l
		go func() {
			<-syncedStatus
			syncedStatus <- currentStatus
		}()
	}
}

func syncHelper(syncedStatus chan Status) {
	syncedStatus <- ready
	for {
		s := <-syncedStatus
		syncedStatus <- s
	}
}

func updateManager(remoteFile string, db *bolt.DB, lastStatus chan Status, updateRequired chan bool) {
	for {
		<-updateRequired
		fmt.Println("Update manager got update requirement")
		performUpdate(remoteFile, db, lastStatus, updateRequired)
	}
}

func updateScheduller(updateRequired chan bool, period int) {
	for {
		fmt.Println("Sending update requirement to chan")
		updateRequired <- true
		fmt.Printf("Next check scheduled after %d Hr\n", period)
		time.Sleep(time.Duration(int64(time.Hour) * int64(period)))
	}
}

func performUpdate(remote string, db *bolt.DB, lastStatus chan Status, updateRequired chan bool) {
	client := &http.Client{}
	req, err := http.NewRequest("HEAD", remote, nil)
	if err != nil {
		fmt.Println(err)
		lastStatus <- failed
		return
	}
	res, err := client.Do(req)
	if err != nil {
		fmt.Println(err)
		lastStatus <- failed
		return
	}
	modified := strings.TrimSpace(res.Header.Get("Last-Modified"))
	fmt.Println("Last-Modified: ", modified)
	t, err := time.Parse(time.RFC1123, modified)
	if err != nil {
		fmt.Println(err)
		lastStatus <- failed
		return
	}
	outdated, err := remoteUpdated(t, db)
	if err != nil {
		fmt.Println(err)
		lastStatus <- failed
		return
	}
	if outdated || err != nil {
		lastStatus <- processing
		success, err := downloadUpdate(remote, db)
		if err != nil || success == false {
			fmt.Println("Download failed with error: ", err)
			lastStatus <- failed
			go func(updateRequired chan bool) { updateRequired <- true }(updateRequired)
			fmt.Println("failed update sent update requirement to the chan")
			return
		}
		fmt.Println("Download succeeded")
		lastStatus <- ready
	}
}

func downloadUpdate(from string, db *bolt.DB) (bool, error) {
	resp, err := http.Get(from)
	if err != nil {
		fmt.Println(err)
		return false, err
	}
	defer resp.Body.Close()

	reader := bzip2.NewReader(resp.Body)
	var line string
	var rdrErr error
	oneBBuf := make([]byte, 1)
	lines := []string{}
	linesRead := 0
	savedLines := 0

	fmt.Println("Starting download")

	for {
		_, rdrErr = reader.Read(oneBBuf)
		if rdrErr != nil {
			fmt.Println(rdrErr)
			break
		}
		switch {
		case '0' <= oneBBuf[0] && oneBBuf[0] <= '9':
			line = line + string(oneBBuf[0])
		case oneBBuf[0] == '\n':
			if line != "" {
				lines = append(lines, line)
				line = ""
				savedLines++
			}
			linesRead++
		}
		if savedLines >= 1000 {
			saveToDB(db, lines)
			savedLines = 0
			lines = []string{}
			if terminal.IsTerminal(int(os.Stdout.Fd())) {
				fmt.Printf("\rDownloading update: %d records processed", linesRead)
			}
		}
	}
	if len(lines) > 0 {
		err := saveToDB(db, lines)
		if err != nil {
			fmt.Println(err)
			return false, err
		}
	}
	fmt.Printf("Download finished: %d records processed\n", linesRead)
	if rdrErr != nil && rdrErr != io.EOF {
		fmt.Println(rdrErr)
		return false, rdrErr
	}
	err = setLastUpdated(db)
	if err != nil {
		fmt.Println(err)
		return false, err
	}
	return true, nil
}

func saveToDB(db *bolt.DB, lines []string) error {
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("passports"))
		if err != nil {
			fmt.Println(err)
			return err
		}
		for _, l := range lines {
			v := b.Get([]byte(strings.TrimSpace(l)))
			if string(v) == "" {
				err = b.Put([]byte(strings.TrimSpace(l)), []byte("1"))
				if err != nil {
					fmt.Println(err)
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		fmt.Println(err)
		return err
	}

	return nil
}

func setLastUpdated(db *bolt.DB) error {
	fmt.Println("Update finished, saving last modification date")
	err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("ustatus"))
		if err != nil {
			fmt.Println(err)
			return err
		}
		b.Put([]byte("updated"), []byte(time.Now().Format(time.RFC1123)))
		return nil
	})
	return err
}

func remoteUpdated(remoteTime time.Time, db *bolt.DB) (bool, error) {
	lastUpdStr := time.RFC1123 // Placeholder for some moment in past
	outdated := true

	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("ustatus"))
		if b == nil {
			return nil
		}
		v := b.Get([]byte("updated"))
		if string(v) == "" {
			return nil
		}
		lastUpdStr = string(v)

		return nil
	})
	lastUpdTime, err := time.Parse(time.RFC1123, lastUpdStr)
	fmt.Println("Remote upd time:", remoteTime, "local upd time:", lastUpdTime)
	if err != nil {
		fmt.Println(err)
		return outdated, err
	}
	if !remoteTime.After(lastUpdTime) {
		fmt.Println("Already up to date")
		outdated = false
		return outdated, err
	}
	return outdated, err
}

func setDBContext(h func(http.ResponseWriter, *http.Request), db *bolt.DB) func(http.ResponseWriter, *http.Request) {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		ctx = context.WithValue(ctx, dbInstance, db)
		h(res, req.WithContext(ctx))
	})
}

func setChannelsContext(h func(http.ResponseWriter, *http.Request), lastStatusChan, syncedStatusChan chan Status, updateRequiredChan chan bool) func(http.ResponseWriter, *http.Request) {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		ctx = context.WithValue(ctx, lastStatus, lastStatusChan)
		ctx = context.WithValue(ctx, syncedStatus, syncedStatusChan)
		ctx = context.WithValue(ctx, updateRequired, updateRequiredChan)
		h(res, req.WithContext(ctx))
	})
}
