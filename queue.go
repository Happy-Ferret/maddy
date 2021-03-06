package maddy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/emersion/maddy/config"
	"github.com/emersion/maddy/log"
	"github.com/emersion/maddy/module"
)

// PartialError describes state of partially successfull message delivery.
type PartialError struct {
	// Recipients for which delivery was successful.
	Successful []string
	// Recipients for which delivery permanently failed.
	Failed []string
	// Recipients for which delivery temporarly failed.
	TemporaryFailed []string

	// Underlying error objects.
	Errs map[string]error
}

func (pe PartialError) Error() string {
	return fmt.Sprintf("delivery failed for some recipients: %v", pe.Errs)
}

type Queue struct {
	name     string
	location string
	wheel    *TimeWheel

	initialRetryTime time.Duration
	retryTimeScale   float64
	maxTries         int

	Log    log.Logger
	Target module.DeliveryTarget

	workersWg sync.WaitGroup
}

type QueueMetadata struct {
	ID string

	Ctx *module.DeliveryContext

	Failed []string

	// Amount of times delivery *already tried*.
	TriesCount  int
	LastAttempt time.Time
}

func NewQueue(_, instName string) (module.Module, error) {
	return &Queue{
		name:             instName,
		initialRetryTime: 15 * time.Minute,
		// note that first **retry** time actually will be initialRetryTime*retryTimeScale
		retryTimeScale: 2,
		Log:            log.Logger{Name: "queue"},
	}, nil
}

func (q *Queue) Init(cfg *config.Map) error {
	q.wheel = NewTimeWheel()
	var workers int

	cfg.Bool("debug", true, &q.Log.Debug)
	cfg.Int("max_tries", false, false, 8, &q.maxTries)
	cfg.Int("workers", false, false, 16, &workers)
	cfg.String("location", false, false, filepath.Join(StateDirectory(cfg.Globals), q.name), &q.location)
	cfg.Custom("target", false, true, nil, deliverDirective, &q.Target)
	if _, err := cfg.Process(); err != nil {
		return err
	}

	if !filepath.IsAbs(q.location) {
		q.location = filepath.Join(StateDirectory(cfg.Globals), q.location)
	}

	// TODO: Check location write permissions.
	if err := os.MkdirAll(q.location, os.ModePerm); err != nil {
		return err
	}
	if err := q.readDiskQueue(); err != nil {
		return err
	}

	q.Log.Debugf("delivery target: %T", q.Target)

	for i := 0; i < workers; i++ {
		q.workersWg.Add(1)
		go q.worker()
	}

	return nil
}

func (q *Queue) Close() error {
	q.wheel.Close()
	q.workersWg.Wait()
	return nil
}

func (q *Queue) worker() {
	for slot := range q.wheel.Dispatch() {
		q.Log.Debugln("worker woke up for", slot.Value)

		q.retryDelivery(slot.Value.(string))
	}
	q.workersWg.Done()
}

func (q *Queue) retryDelivery(id string) {
	meta, body, err := q.openMessage(id)
	if err != nil {
		q.Log.Printf("failed to read message: %v", err)
	}
	defer body.Close()

	q.Log.Debugln("try count", meta.TriesCount)

	shouldRetry, newRcpts := q.attemptDelivery(&meta, body)
	if !shouldRetry {
		q.removeFromDisk(id)
		return
	}
	meta.Ctx.To = newRcpts

	if meta.TriesCount == q.maxTries || len(meta.Ctx.To) == 0 {
		q.removeFromDisk(id)
		return
	}

	meta.TriesCount += 1
	meta.LastAttempt = time.Now()

	if err := q.updateMetadataOnDisk(&meta); err != nil {
		q.Log.Printf("failed to update meta-data: %v", err)
	}

	nextTryTime := time.Now()
	nextTryTime = nextTryTime.Add(q.initialRetryTime * time.Duration(math.Pow(q.retryTimeScale, float64(meta.TriesCount))))
	q.Log.Printf("will retry in %v (at %v)", time.Until(nextTryTime), nextTryTime)

	q.wheel.Add(nextTryTime, id)
}

func (q *Queue) attemptDelivery(meta *QueueMetadata, body io.Reader) (shouldRetry bool, newRcpts []string) {
	meta.Ctx.Ctx["id"] = meta.ID
	err := q.Target.Deliver(*meta.Ctx, body)

	if err == nil {
		q.Log.Debugf("success for %s", meta.ID)
		return false, nil
	}

	if partialErr, ok := err.(PartialError); ok {
		meta.Failed = append(meta.Failed, partialErr.Failed...)
		q.Log.Debugf("partial failure for %s:", meta.ID)
		q.Log.Debugf("- successful: %v", partialErr.Successful)
		q.Log.Debugf("- temporary failed: %v", partialErr.TemporaryFailed)
		q.Log.Debugf("- permanently failed: %v", partialErr.Failed)
		q.Log.Debugf("- errors: %+v", partialErr.Errs)
		return len(partialErr.TemporaryFailed) != 0, partialErr.TemporaryFailed
	}

	// non-PartialErr is assumed to be permanent failure for all recipients
	q.Log.Debugf("permanent failure for all recipients of %s: %v", meta.ID, err)
	meta.Failed = append(meta.Failed, meta.Ctx.To...)
	return false, nil
}

func filterRcpts(ctx *module.DeliveryContext) error {
	newRcpts := make([]string, 0, len(ctx.To))
	for _, rcpt := range ctx.To {
		parts := strings.Split(rcpt, "@")
		if len(parts) != 2 {
			return fmt.Errorf("malformed address %s: missing domain part", rcpt)
		}

		if _, ok := ctx.Opts["local_only"]; ok {
			hostname := ctx.Opts["hostname"]
			if hostname == "" {
				hostname = ctx.OurHostname
			}

			if parts[1] != hostname {
				log.Debugf("local_only, skipping %s", rcpt)
				continue
			}
		}
		if _, ok := ctx.Opts["remote_only"]; ok {
			hostname := ctx.Opts["hostname"]
			if hostname == "" {
				hostname = ctx.OurHostname
			}

			if parts[1] == hostname {
				log.Debugf("remote_only, skipping %s", rcpt)
				continue
			}
		}

		newRcpts = append(newRcpts, rcpt)
	}

	ctx.To = newRcpts
	return nil
}

func (q *Queue) Deliver(ctx module.DeliveryContext, msg io.Reader) error {
	if err := filterRcpts(&ctx); err != nil {
		return err
	}

	if len(ctx.To) == 0 {
		return nil
	}
	id := ctx.DeliveryID

	var body io.ReadSeeker
	if seekable, ok := msg.(io.ReadSeeker); ok {
		body = seekable
	} else {
		bodySlice, err := ioutil.ReadAll(msg)
		if err != nil {
			return errors.New("failed to buffer message")
		}
		body = bytes.NewReader(bodySlice)
	}

	go func() {
		q.Log.Printf("enqueued message %s from %s (%s)", id, ctx.SrcAddr, ctx.SrcHostname)
		meta := &QueueMetadata{
			ID:          id,
			TriesCount:  1,
			Ctx:         &ctx,
			LastAttempt: time.Now(),
		}
		shouldRetry, newRcpts := q.attemptDelivery(meta, body)
		if !shouldRetry {
			return
		}

		meta.Ctx.To = newRcpts
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			q.Log.Printf("failed to rewind message buffer: %v", err)
		}
		if err := q.storeNewMessage(meta, body); err != nil {
			q.Log.Printf("failed to save message %v to disk: %v", id, err)
			return
		}
		q.Log.Printf("will retry in %v (at %v)", q.initialRetryTime, time.Now().Add(q.initialRetryTime))
		q.wheel.Add(time.Now().Add(q.initialRetryTime), meta.ID)
	}()

	return nil
}

func (q *Queue) removeFromDisk(id string) {
	bodyPath := filepath.Join(q.location, id)
	if err := os.Remove(bodyPath); err != nil {
		q.Log.Printf("failed to remove %s body from disk", id)
	}
	metaPath := bodyPath + ".meta"
	if err := os.Remove(metaPath); err != nil {
		q.Log.Printf("failed to remove %s meta-data from disk", id)
	}
}

func (q *Queue) readDiskQueue() error {
	dirInfo, err := ioutil.ReadDir(q.location)
	if err != nil {
		return err
	}

	loadedCount := 0
	for _, entry := range dirInfo {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta") {
			continue
		}
		id := entry.Name()[:len(entry.Name())-5]
		if _, err := os.Stat(filepath.Join(q.location, id)); err != nil {
			q.Log.Printf("failed to stat body file %s, removing meta-data file", id)
			os.Remove(entry.Name())
			continue
		}

		meta, err := q.readMessageMeta(id)
		if err != nil {
			q.Log.Printf("failed to read meta-data for %s, skipping: %v", id, err)
			continue
		}

		nextTryTime := meta.LastAttempt
		nextTryTime = nextTryTime.Add(q.initialRetryTime * time.Duration(math.Pow(q.retryTimeScale, float64(meta.TriesCount-1))))

		if nextTryTime.Before(time.Now()) {
			nextTryTime = time.Now().Add(5 * time.Second)
		}

		q.Log.Debugf("will try to deliver %s in %v (%v)", id, time.Until(nextTryTime), nextTryTime)
		q.wheel.Add(nextTryTime, id)
		loadedCount++
	}

	if loadedCount != 0 {
		q.Log.Printf("loaded %d saved queue entries", loadedCount)
	}

	return nil
}

func (q *Queue) storeNewMessage(meta *QueueMetadata, body io.Reader) error {
	bodyPath := filepath.Join(q.location, meta.ID)
	bodyFile, err := os.Create(bodyPath)
	if err != nil {
		return err
	}
	defer bodyFile.Close()
	if _, err := io.Copy(bodyFile, body); err != nil {
		return err
	}

	if err := q.updateMetadataOnDisk(meta); err != nil {
		os.Remove(bodyPath)
		return err
	}

	return nil
}

func (q *Queue) updateMetadataOnDisk(meta *QueueMetadata) error {
	metaPath := filepath.Join(q.location, meta.ID+".meta")
	file, err := os.Create(metaPath)
	if err != nil {
		return err
	}
	defer file.Close()

	if _, ok := meta.Ctx.SrcAddr.(*net.TCPAddr); !ok {
		meta.Ctx.SrcAddr = nil
	}

	if err := json.NewEncoder(file).Encode(meta); err != nil {
		return err
	}
	return nil
}

func (q *Queue) readMessageMeta(id string) (QueueMetadata, error) {
	metaPath := filepath.Join(q.location, id+".meta")
	file, err := os.Open(metaPath)
	if err != nil {
		return QueueMetadata{}, err
	}
	defer file.Close()

	meta := QueueMetadata{}

	// net.Addr can't be deserialized because we don't know concrete type. For
	// this reason we assume that SrcAddr is TCPAddr, if it is not - we drop it
	// during serialization (see updateMetadataOnDisk).
	meta.Ctx = &module.DeliveryContext{}
	meta.Ctx.SrcAddr = &net.TCPAddr{}

	if err := json.NewDecoder(file).Decode(&meta); err != nil {
		return QueueMetadata{}, err
	}

	return meta, nil
}

func (q *Queue) openMessage(id string) (meta QueueMetadata, body *os.File, err error) {
	meta, err = q.readMessageMeta(id)
	if err != nil {
		return
	}

	bodyPath := filepath.Join(q.location, id)
	body, err = os.Open(bodyPath)
	if err != nil {
		q.Log.Printf("removing dangling meta-data file for %v", id)
		os.Remove(filepath.Join(q.location, id))
	}

	return
}

func (q *Queue) InstanceName() string {
	return q.name
}

func (q *Queue) Name() string {
	return "queue"
}

func init() {
	module.Register("queue", NewQueue)
}
