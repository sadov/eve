// Copyright (c) 2018 Zededa, Inc.
// All rights reserved.

package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	google_protobuf "github.com/golang/protobuf/ptypes/timestamp"
	"github.com/satori/go.uuid"
	"github.com/zededa/api/zmet"
	"github.com/zededa/go-provision/agentlog"
	"github.com/zededa/go-provision/flextimer"
	"github.com/zededa/go-provision/pidfile"
	"github.com/zededa/go-provision/pubsub"
	"github.com/zededa/go-provision/types"
	"github.com/zededa/go-provision/watch"
	"github.com/zededa/go-provision/zboot"
	"github.com/zededa/go-provision/zedcloud"
	"io"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	agentName       = "logmanager"
	identityDirname = "/config"
	serverFilename  = identityDirname + "/server"
	uuidFileName    = identityDirname + "/uuid"
	DNSDirname      = "/var/run/zedrouter/DeviceNetworkStatus"
	xenLogDirname   = "/var/log/xen"
)

var devUUID uuid.UUID
var deviceNetworkStatus types.DeviceNetworkStatus
var debug bool
var serverName string
var logsApi string = "api/v1/edgedevice/logs"
var logsUrl string
var zedcloudCtx zedcloud.ZedCloudContext

var logMaxSize = 100

// Key is ifname string
var logs map[string]zedcloudLogs

// global stuff
type logDirModifyHandler func(ctx interface{}, logFileName string, source string)
type logDirDeleteHandler func(ctx interface{}, logFileName string, source string)

// Set from Makefile
var Version = "No version specified"

// Based on the proto file
type logEntry struct {
	severity  string
	source    string // basename of filename?
	iid       string // XXX e.g. PID - where do we get it from?
	content   string // One line
	timestamp *google_protobuf.Timestamp
}

// List of log files we watch
type loggerContext struct {
	logfileReaders []logfileReader
	image          string
	logChan        chan<- logEntry
}

type logfileReader struct {
	filename string
	source   string
	fileDesc *os.File
	reader   *bufio.Reader
	size     int64 // To detect file truncation
}

// These are for the case when we have a separate channel/image
// per file.
type imageLogfileReader struct {
	logfileReader
	image   string
	logChan chan logEntry
}

// List of log files we watch where channel/image is per file
type imageLoggerContext struct {
	logfileReaders []imageLogfileReader
}

// Context for handleDNSModify
type DNSContext struct {
	usableAddressCount int
}

type zedcloudLogs struct {
	FailureCount uint64
	SuccessCount uint64
	LastFailure  time.Time
	LastSuccess  time.Time
}

func main() {
	// Note that device-steps.sh sends our output to /var/run
	// so we don't log our own output.
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC)

	defaultLogdirname := agentlog.GetCurrentLogdir()
	versionPtr := flag.Bool("v", false, "Version")
	debugPtr := flag.Bool("d", false, "Debug")
	forcePtr := flag.Bool("f", false, "Force")
	logdirPtr := flag.String("l", defaultLogdirname, "Log file directory")
	flag.Parse()
	debug = *debugPtr
	logDirName := *logdirPtr
	force := *forcePtr
	if *versionPtr {
		fmt.Printf("%s: %s\n", os.Args[0], Version)
		return
	}

	// Note that LISP needs a separate directory since it moves
	// old content to a subdir when it (re)starts
	lispLogDirName := fmt.Sprintf("%s/%s", logDirName, "lisp")
	if err := pidfile.CheckAndCreatePidfile(agentName); err != nil {
		log.Fatal(err)
	}
	log.Printf("Starting %s watching %s\n", agentName, logDirName)
	log.Printf("watching %s\n", lispLogDirName)

	cms := zedcloud.GetCloudMetrics() // Need type of data
	pub, err := pubsub.Publish(agentName, cms)
	if err != nil {
		log.Fatal(err)
	}

	// Wait until we have at least one useable address?
	DNSctx := DNSContext{}
	DNSctx.usableAddressCount = types.CountLocalAddrAnyNoLinkLocal(deviceNetworkStatus)

	networkStatusChanges := make(chan string)
	go watch.WatchStatus(DNSDirname, networkStatusChanges)

	log.Printf("Waiting until we have some uplinks with usable addresses\n")
	for types.CountLocalAddrAnyNoLinkLocal(deviceNetworkStatus) == 0 && !force {
		select {
		case change := <-networkStatusChanges:
			watch.HandleStatusEvent(change, &DNSctx,
				DNSDirname,
				&types.DeviceNetworkStatus{},
				handleDNSModify, handleDNSDelete,
				nil)
		}
	}

	//Get servername, set logUrl, get device id and initialize zedcloudCtx
	sendCtxInit()

	// Publish send metrics for zedagent every 10 seconds
	interval := time.Duration(10 * time.Second)
	max := float64(interval)
	min := max * 0.3
	publishTimer := flextimer.NewRangeTicker(time.Duration(min),
		time.Duration(max))

	currentPartition := zboot.GetCurrentPartition()
	loggerChan := make(chan logEntry)
	ctx := loggerContext{logChan: loggerChan, image: currentPartition}
	xenCtx := imageLoggerContext{}

	// Start sender of log events
	go processEvents(currentPartition, loggerChan)

	// The OtherPartition files will not change hence we can just
	// read them and send their lines; no need to watch for changes.
	otherLogdirname := agentlog.GetOtherLogdir()
	if otherLogdirname != "" {
		log.Printf("Have logs from failed upgrade in %s\n",
			otherLogdirname)
		otherLoggerChan := make(chan logEntry)
		otherPartition := zboot.GetOtherPartition()
		go processEvents(otherPartition, otherLoggerChan)
		files, err := ioutil.ReadDir(otherLogdirname)
		if err != nil {
			log.Fatal(err)
		}
		for _, file := range files {
			filename := otherLogdirname + "/" + file.Name()
			if !strings.HasSuffix(filename, ".log") {
				log.Printf("Ignore %s\n", filename)
				continue
			}
			log.Printf("Read %s until EOF\n", filename)
			name := strings.Split(file.Name(), ".log")
			source := name[0]
			logReader(filename, source, otherLoggerChan)
		}
		// make processEvents() exit for this channel
		close(otherLoggerChan)
	}

	logDirChanges := make(chan string)
	go watch.WatchStatus(logDirName, logDirChanges)

	lispLogDirChanges := make(chan string)
	go watch.WatchStatus(lispLogDirName, lispLogDirChanges)

	xenLogDirChanges := make(chan string)
	go watch.WatchStatus(xenLogDirname, xenLogDirChanges)

	log.Println("called watcher...")
	for {
		select {

		case change := <-logDirChanges:
			HandleLogDirEvent(change, logDirName, &ctx,
				handleLogDirModify, handleLogDirDelete)

		case change := <-lispLogDirChanges:
			HandleLogDirEvent(change, lispLogDirName, &ctx,
				handleLogDirModify, handleLogDirDelete)

		case change := <-xenLogDirChanges:
			HandleLogDirEvent(change, xenLogDirname, &xenCtx,
				handleXenLogDirModify, handleXenLogDirDelete)

		case change := <-networkStatusChanges:
			watch.HandleStatusEvent(change, &DNSctx,
				DNSDirname,
				&types.DeviceNetworkStatus{},
				handleDNSModify, handleDNSDelete,
				nil)

		case <-publishTimer.C:
			if debug {
				log.Println("publishTimer at",
					time.Now())
			}
			pub.Publish("global", zedcloud.GetCloudMetrics())
		}
	}
}

func handleDNSModify(ctxArg interface{}, statusFilename string,
	statusArg interface{}) {
	status := statusArg.(*types.DeviceNetworkStatus)
	ctx := ctxArg.(*DNSContext)

	if statusFilename != "global" {
		log.Printf("handleDNSModify: ignoring %s\n", statusFilename)
		return
	}
	log.Printf("handleDNSModify for %s\n", statusFilename)
	deviceNetworkStatus = *status
	newAddrCount := types.CountLocalAddrAnyNoLinkLocal(deviceNetworkStatus)
	ctx.usableAddressCount = newAddrCount
	log.Printf("handleDNSModify done for %s; %d usable\n",
		statusFilename, newAddrCount)
}

func handleDNSDelete(ctxArg interface{}, statusFilename string) {
	log.Printf("handleDNSDelete for %s\n", statusFilename)
	ctx := ctxArg.(*DNSContext)

	if statusFilename != "global" {
		log.Printf("handleDNSDelete: ignoring %s\n", statusFilename)
		return
	}
	deviceNetworkStatus = types.DeviceNetworkStatus{}
	newAddrCount := types.CountLocalAddrAnyNoLinkLocal(deviceNetworkStatus)
	ctx.usableAddressCount = newAddrCount
	log.Printf("handleDNSDelete done for %s\n", statusFilename)
}

// This runs as a separate go routine sending out data
func processEvents(image string, logChan <-chan logEntry) {

	reportLogs := new(zmet.LogBundle)
	// XXX should we make the log interval configurable?
	interval := time.Duration(10 * time.Second)
	max := float64(interval)
	min := max * 0.3
	flushTimer := flextimer.NewRangeTicker(time.Duration(min),
		time.Duration(max))
	counter := 0

	for {
		select {
		case event, more := <-logChan:
			if !more {
				log.Printf("processEvents: %s end\n", image)
				if counter > 0 {
					sendProtoStrForLogs(reportLogs, image,
						iteration)
				}
				return
			}
			HandleLogEvent(event, reportLogs, counter)
			counter++

			if counter >= logMaxSize {
				sendProtoStrForLogs(reportLogs, image,
					iteration)
				counter = 0
				iteration += 1
			}

		case <-flushTimer.C:
			if debug {
				log.Printf("Logger Flush at %v %v\n",
					image, reportLogs.Timestamp)
			}
			if counter > 0 {
				sendProtoStrForLogs(reportLogs, image,
					iteration)
				counter = 0
				iteration += 1
			}
		}
	}
}

var msgIdCounter = 1
var iteration = 0

func HandleLogEvent(event logEntry, reportLogs *zmet.LogBundle, counter int) {
	// Assign a unique msgId for each message
	msgId := msgIdCounter
	msgIdCounter += 1
	if debug {
		fmt.Printf("Read event from %s time %v id %d: %s\n",
			event.source, event.timestamp, msgId, event.content)
	}
	logDetails := &zmet.LogEntry{}
	logDetails.Content = event.content
	logDetails.Timestamp = event.timestamp
	logDetails.Source = event.source
	logDetails.Iid = event.iid
	logDetails.Msgid = uint64(msgId)
	reportLogs.Log = append(reportLogs.Log, logDetails)
	// XXX count bytes instead of messages? Limit to << 64k
}

func sendProtoStrForLogs(reportLogs *zmet.LogBundle, image string,
	iteration int) {
	reportLogs.Timestamp = ptypes.TimestampNow()
	reportLogs.DevID = *proto.String(devUUID.String())
	reportLogs.Image = image

	if debug {
		log.Println("sendProtoStrForLogs called...", iteration)
	}
	data, err := proto.Marshal(reportLogs)
	if err != nil {
		log.Fatal("sendProtoStrForLogs proto marshaling error: ", err)
	}
	if debug {
		log.Printf("Log Details (len %d): %s\n", len(data), reportLogs)
	}
	buf := bytes.NewBuffer(data)
	if buf == nil {
		log.Fatal("sendProtoStrForLogs malloc error:")
	}

	_, _, err = zedcloud.SendOnAllIntf(zedcloudCtx, logsUrl,
		int64(len(data)), buf, iteration)
	if err != nil {
		// XXX need to queue message and retry
		// For now we discard what failed to constrain the size
		// of each message
		log.Printf("SendProtoStrForLogs %d bytes image %s failed: %s\n",
			len(data), image, err)
		reportLogs.Log = []*zmet.LogEntry{}
		return
	}
	log.Printf("Sent %d bytes image %s to %s\n", len(data), image, logsUrl)
	reportLogs.Log = []*zmet.LogEntry{}
}

func sendCtxInit() {
	//get server name
	bytes, err := ioutil.ReadFile(serverFilename)
	if err != nil {
		log.Fatal(err)
	}
	strTrim := strings.TrimSpace(string(bytes))
	serverName = strings.Split(strTrim, ":")[0]

	//set log url
	logsUrl = serverName + "/" + logsApi

	tlsConfig, err := zedcloud.GetTlsConfig(serverName, nil)
	if err != nil {
		log.Fatal(err)
	}
	zedcloudCtx.DeviceNetworkStatus = &deviceNetworkStatus
	zedcloudCtx.TlsConfig = tlsConfig
	zedcloudCtx.Debug = debug
	zedcloudCtx.FailureFunc = zedcloud.ZedCloudFailure
	zedcloudCtx.SuccessFunc = zedcloud.ZedCloudSuccess

	b, err := ioutil.ReadFile(uuidFileName)
	if err != nil {
		log.Fatal("ReadFile", err, uuidFileName)
	}
	uuidStr := strings.TrimSpace(string(b))
	devUUID, err = uuid.FromString(uuidStr)
	if err != nil {
		log.Fatal("uuid.FromString", err, string(b))
	}
	fmt.Printf("Read UUID %s\n", devUUID)
}

func HandleLogDirEvent(change string, logDirName string, ctx interface{},
	handleLogDirModifyFunc logDirModifyHandler,
	handleLogDirDeleteFunc logDirDeleteHandler) {

	operation := string(change[0])
	fileName := string(change[2:])
	if !strings.HasSuffix(fileName, ".log") {
		log.Printf("Ignoring file <%s> operation %s\n",
			fileName, operation)
		return
	}
	logFilePath := logDirName + "/" + fileName
	// Remove .log from name
	name := strings.Split(fileName, ".log")
	source := name[0]
	if operation == "D" {
		handleLogDirDeleteFunc(ctx, logFilePath, source)
		return
	}
	if operation != "M" {
		log.Fatal("Unknown operation from Watcher: ",
			operation)
	}
	handleLogDirModifyFunc(ctx, logFilePath, source)
}

func handleXenLogDirModify(context interface{},
	filename string, source string) {

	if strings.Compare(source, "hypervisor") == 0 {
		if debug {
			log.Println("Ignoring hypervisor log while sending domU log")
		}
		return
	}
	ctx := context.(*imageLoggerContext)
	for i, r := range ctx.logfileReaders {
		if r.filename == filename {
			readLineToEvent(&ctx.logfileReaders[i].logfileReader,
				r.logChan)
			return
		}
	}
	createXenLogger(ctx, filename, source)
}

func createXenLogger(ctx *imageLoggerContext, filename string, source string) {

	log.Printf("createXenLogger: add %s, source %s\n", filename, source)

	fileDesc, err := os.Open(filename)
	if err != nil {
		log.Printf("Log file ignored due to %s\n", err)
		return
	}
	// Start reading from the file with a reader.
	reader := bufio.NewReader(fileDesc)
	if reader == nil {
		log.Printf("Log file ignored due to %s\n", err)
		return
	}

	r0 := logfileReader{filename: filename,
		source:   source,
		fileDesc: fileDesc,
		reader:   reader,
	}
	r := imageLogfileReader{logfileReader: r0,
		image:   source,
		logChan: make(chan logEntry),
	}

	// process associated channel
	go processEvents(source, r.logChan)

	// read initial entries until EOF
	readLineToEvent(&r.logfileReader, r.logChan)
	ctx.logfileReaders = append(ctx.logfileReaders, r)
}

func handleXenLogDirDelete(context interface{},
	filename string, source string) {
	ctx := context.(*imageLoggerContext)

	log.Printf("handleLogDirDelete: delete %s, source %s\n", filename, source)
	for _, logger := range ctx.logfileReaders {
		if logger.logfileReader.filename == filename {
			// XXX:FIXME, delete the entry
		}
	}
}

// If the filename is new we spawn a go routine which will read
func handleLogDirModify(context interface{}, filename string, source string) {
	ctx := context.(*loggerContext)

	for i, r := range ctx.logfileReaders {
		if r.filename == filename {
			readLineToEvent(&ctx.logfileReaders[i], ctx.logChan)
			return
		}
	}
	createLogger(ctx, filename, source)
}

func createLogger(ctx *loggerContext, filename, source string) {

	log.Printf("createLogger: add %s, source %s\n", filename, source)

	fileDesc, err := os.Open(filename)
	if err != nil {
		log.Printf("Log file ignored due to %s\n", err)
		return
	}
	// Start reading from the file with a reader.
	reader := bufio.NewReader(fileDesc)
	if reader == nil {
		log.Printf("Log file ignored due to %s\n", err)
		return
	}
	r := logfileReader{filename: filename,
		source:   source,
		fileDesc: fileDesc,
		reader:   reader,
	}
	// read initial entries until EOF
	readLineToEvent(&r, ctx.logChan)
	ctx.logfileReaders = append(ctx.logfileReaders, r)
}

// XXX TBD should we stop the go routine?
func handleLogDirDelete(ctx interface{}, filename string, source string) {
	// ctx := context.(*loggerContext)
}

// Read until EOF or error
func readLineToEvent(r *logfileReader, logChan chan<- logEntry) {
	// Check if shrunk aka truncated
	fi, err := r.fileDesc.Stat()
	if err != nil {
		log.Printf("Stat failed %s\n", err)
		return
	}
	if fi.Size() < r.size {
		log.Printf("File shrunk from %d to %d\n", r.size, fi.Size())
		_, err = r.fileDesc.Seek(0, os.SEEK_SET)
		if err != nil {
			log.Printf("Seek failed %s\n", err)
			return
		}
	}
	for {
		line, err := r.reader.ReadString('\n')
		if err != nil {
			// XXX do we need to look for file truncation during
			// this loop?
			if debug {
				log.Println(err)
			}
			if err != io.EOF {
				fmt.Printf(" > Failed!: %v\n", err)
			}
			break
		}
		// remove trailing "/n" from line
		line = line[0 : len(line)-1]
		// XXX parse timestamp and remove it from line (if present)
		// otherwise leave timestamp unitialized
		parsedDateAndTime, err := parseDateTime(line)
		// XXX set iid to PID?
		if err != nil {
			logChan <- logEntry{source: r.source, content: line}
		} else {
			logChan <- logEntry{source: r.source, content: line,
				timestamp: parsedDateAndTime}
		}

	}
	// Update size
	fi, err = r.fileDesc.Stat()
	if err != nil {
		log.Printf("Stat failed %s\n", err)
		return
	}
	r.size = fi.Size()
}

//parse date and time from agent logs
func parseDateTime(line string) (*google_protobuf.Timestamp, error) {

	var protoDateAndTime *google_protobuf.Timestamp
	re := regexp.MustCompile(`^\d{4}/\d{2}/\d{2}`)
	matched := re.MatchString(line)
	if matched {
		dateAndTime := strings.Split(line, " ")
		re := regexp.MustCompile("/")
		newDateFormat := re.ReplaceAllLiteralString(dateAndTime[0], "-")

		timeFormat := strings.Split(dateAndTime[1], ".")[0]
		newDateAndTime := newDateFormat + "T" + timeFormat
		layout := "2006-01-02T15:04:05"

		///convert newDateAndTime type string to type time.time
		dt, err := time.Parse(layout, newDateAndTime)
		if err != nil {
			log.Println(err)
			return nil, err
		} else {
			//convert dt type time.time to type proto
			protoDateAndTime, err = ptypes.TimestampProto(dt)
			if err != nil {
				log.Println("Error while converting timestamp in proto format: ", err)
				return nil, err
			} else {
				return protoDateAndTime, nil
			}
		}
	} else {
		return nil, errors.New("date and time format not found")
	}
}

// Read unchanging files until EOF
// Used for the otherpartition files!
func logReader(logFile string, source string, logChan chan<- logEntry) {
	fileDesc, err := os.Open(logFile)
	if err != nil {
		log.Printf("Log file ignored due to %s\n", err)
		return
	}
	// Start reading from the file with a reader.
	reader := bufio.NewReader(fileDesc)
	if reader == nil {
		log.Printf("Log file ignored due to %s\n", err)
		return
	}
	r := logfileReader{filename: logFile,
		source:   source,
		fileDesc: fileDesc,
		reader:   reader,
	}
	// read entries until EOF
	readLineToEvent(&r, logChan)
	log.Printf("logReader done for %s\n", logFile)
}