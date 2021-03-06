package tailx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/json-iterator/go"

	"github.com/qiniu/log"

	"github.com/qiniu/logkit/conf"
	"github.com/qiniu/logkit/reader"
	. "github.com/qiniu/logkit/utils/models"
)

func init() {
	reader.RegisterConstructor(reader.ModeTailx, NewReader)
}

type Reader struct {
	started     bool
	status      int32
	fileReaders map[string]*ActiveReader
	armapmux    sync.Mutex
	startmux    sync.Mutex
	curFile     string
	headRegexp  *regexp.Regexp
	cacheMap    map[string]string

	msgChan chan Result
	errChan chan error

	//以下为传入参数
	meta           *reader.Meta
	logPathPattern string
	expire         time.Duration
	statInterval   time.Duration
	maxOpenFiles   int
	whence         string

	stats     StatsInfo
	statsLock sync.RWMutex
}

type ActiveReader struct {
	cacheLineMux sync.RWMutex
	br           *reader.BufReader
	realpath     string
	originpath   string
	readcache    string
	msgchan      chan<- Result
	errChan      chan<- error
	status       int32
	inactive     int32 //当inactive>0 时才会被expire回收
	runnerName   string

	emptyLineCnt int

	stats     StatsInfo
	statsLock sync.RWMutex
}

type Result struct {
	result  string
	logpath string
}

func NewActiveReader(originPath, realPath, whence string, meta *reader.Meta, msgChan chan<- Result, errChan chan<- error) (ar *ActiveReader, err error) {
	rpath := strings.Replace(realPath, string(os.PathSeparator), "_", -1)
	subMetaPath := filepath.Join(meta.Dir, rpath)
	subMeta, err := reader.NewMeta(subMetaPath, subMetaPath, realPath, reader.ModeFile, meta.TagFile, reader.DefautFileRetention)
	if err != nil {
		return nil, err
	}
	subMeta.Readlimit = meta.Readlimit
	//tailx模式下新增runner是因为文件已经感知到了，所以不可能文件不存在，那么如果读取还遇到错误，应该马上返回，所以errDirectReturn=true
	fr, err := reader.NewSingleFile(subMeta, realPath, whence, true)
	if err != nil {
		return
	}
	bf, err := reader.NewReaderSize(fr, subMeta, reader.DefaultBufSize)
	if err != nil {
		return
	}
	return &ActiveReader{
		cacheLineMux: sync.RWMutex{},
		br:           bf,
		realpath:     realPath,
		originpath:   originPath,
		msgchan:      msgChan,
		errChan:      errChan,
		inactive:     1,
		emptyLineCnt: 0,
		runnerName:   meta.RunnerName,
		status:       reader.StatusInit,
		statsLock:    sync.RWMutex{},
	}, nil

}

func (ar *ActiveReader) Run() {
	if !atomic.CompareAndSwapInt32(&ar.status, reader.StatusInit, reader.StatusRunning) {
		log.Errorf("Runner[%v] ActiveReader %s was not in StatusInit before Running,exit it...", ar.runnerName, ar.originpath)
		return
	}
	var err error
	timer := time.NewTicker(time.Second)
	for {
		if atomic.LoadInt32(&ar.status) == reader.StatusStopped || atomic.LoadInt32(&ar.status) == reader.StatusStopping {
			atomic.CompareAndSwapInt32(&ar.status, reader.StatusStopping, reader.StatusStopped)
			log.Warnf("Runner[%v] ActiveReader %s was stopped", ar.runnerName, ar.originpath)
			return
		}
		if ar.readcache == "" {
			ar.cacheLineMux.Lock()
			ar.readcache, err = ar.br.ReadLine()
			ar.cacheLineMux.Unlock()
			if err != nil && err != io.EOF {
				log.Warnf("Runner[%v] ActiveReader %s read error: %v", ar.runnerName, ar.originpath, err)
				ar.setStatsError(err.Error())
				ar.sendError(err)
				time.Sleep(3 * time.Second)
				continue
			}
			if ar.readcache == "" {
				ar.emptyLineCnt++
				//文件EOF，同时没有任何内容，代表不是第一次EOF，休息时间设置长一些
				if err == io.EOF {
					atomic.StoreInt32(&ar.inactive, 1)
					log.Debugf("Runner[%v] %v meet EOF, ActiveReader was inactive now, sleep 5 seconds", ar.runnerName, ar.originpath)
					time.Sleep(5 * time.Second)
					continue
				}
				// 一小时没读到内容，设置为inactive
				if ar.emptyLineCnt > 60*60 {
					atomic.StoreInt32(&ar.inactive, 1)
				}
				//读取的结果为空，无论如何都sleep 1s
				time.Sleep(time.Second)
				continue
			}
		}
		log.Debugf("Runner[%v] %v >>>>>>readcache <%v> linecache <%v>", ar.runnerName, ar.originpath, ar.readcache, string(ar.br.FormMutiLine()))
		repeat := 0
		for {
			if ar.readcache == "" {
				break
			}
			repeat++
			if repeat%3000 == 0 {
				log.Errorf("Runner[%v] %v ActiveReader has timeout 3000 times with readcache %v", ar.runnerName, ar.originpath, ar.readcache)
			}

			atomic.StoreInt32(&ar.inactive, 0)
			ar.emptyLineCnt = 0
			//做这一层结构为了快速结束
			if atomic.LoadInt32(&ar.status) == reader.StatusStopped || atomic.LoadInt32(&ar.status) == reader.StatusStopping {
				log.Debugf("Runner[%v] %v ActiveReader was stopped when waiting to send data", ar.runnerName, ar.originpath)
				atomic.CompareAndSwapInt32(&ar.status, reader.StatusStopping, reader.StatusStopped)
				return
			}
			select {
			case ar.msgchan <- Result{result: ar.readcache, logpath: ar.originpath}:
				ar.cacheLineMux.Lock()
				ar.readcache = ""
				ar.cacheLineMux.Unlock()
			case <-timer.C:
			}
		}
	}
}
func (ar *ActiveReader) Close() error {
	defer log.Warnf("Runner[%v] ActiveReader %s was closed", ar.runnerName, ar.originpath)
	err := ar.br.Close()
	if atomic.CompareAndSwapInt32(&ar.status, reader.StatusRunning, reader.StatusStopping) {
		log.Warnf("Runner[%v] ActiveReader %s was closing", ar.runnerName, ar.originpath)
	} else {
		return err
	}

	cnt := 0
	// 等待结束
	for atomic.LoadInt32(&ar.status) != reader.StatusStopped {
		cnt++
		//超过300个10ms，即3s，就强行退出
		if cnt > 300 {
			log.Errorf("Runner[%v] ActiveReader %s was not closed after 3s, force closing it", ar.runnerName, ar.originpath)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}

func (ar *ActiveReader) setStatsError(err string) {
	ar.statsLock.Lock()
	defer ar.statsLock.Unlock()
	ar.stats.LastError = err
}

func (ar *ActiveReader) sendError(err error) {
	if err == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Runner[%v] ActiveReader %s Recovered from %v", ar.runnerName, ar.originpath, r)
		}
	}()
	ar.errChan <- err
}

func (ar *ActiveReader) Status() StatsInfo {
	ar.statsLock.RLock()
	defer ar.statsLock.RUnlock()
	return ar.stats
}

func (ar *ActiveReader) Lag() (rl *LagInfo, err error) {
	return ar.br.Lag()
}

//除了sync自己的bufreader，还要sync一行linecache
func (ar *ActiveReader) SyncMeta() string {
	ar.cacheLineMux.Lock()
	defer ar.cacheLineMux.Unlock()
	ar.br.SyncMeta()
	return ar.readcache
}

func (ar *ActiveReader) expired(expireDur time.Duration) bool {
	fi, err := os.Stat(ar.realpath)
	if err != nil {
		if os.IsNotExist(err) {
			return true
		}
		log.Errorf("Runner[%v] stat log %v error %v, will not expire it...", ar.runnerName, ar.originpath, err)
		return false
	}
	if fi.ModTime().Add(expireDur).Before(time.Now()) && atomic.LoadInt32(&ar.inactive) > 0 {
		return true
	}
	return false
}

func NewReader(meta *reader.Meta, conf conf.MapConf) (mr reader.Reader, err error) {
	logPathPattern, err := conf.GetString(reader.KeyLogPath)
	if err != nil {
		return
	}
	whence, _ := conf.GetStringOr(reader.KeyWhence, reader.WhenceOldest)

	expireDur, _ := conf.GetStringOr(reader.KeyExpire, "24h")
	statIntervalDur, _ := conf.GetStringOr(reader.KeyStatInterval, "3m")
	maxOpenFiles, _ := conf.GetIntOr(reader.KeyMaxOpenFiles, 256)

	expire, err := time.ParseDuration(expireDur)
	if err != nil {
		return nil, err
	}
	statInterval, err := time.ParseDuration(statIntervalDur)
	if err != nil {
		return nil, err
	}
	_, _, bufsize, err := meta.ReadBufMeta()
	if err != nil {
		if os.IsNotExist(err) {
			log.Debugf("Runner[%v] %v recover from meta error %v, ignore...", meta.RunnerName, logPathPattern, err)
		} else {
			log.Warnf("Runner[%v] %v recover from meta error %v, ignore...", meta.RunnerName, logPathPattern, err)
		}
		bufsize = 0
		err = nil
	}

	cacheMap := make(map[string]string)
	buf := make([]byte, bufsize)
	if bufsize > 0 {
		if _, err = meta.ReadBuf(buf); err != nil {
			if os.IsNotExist(err) {
				log.Debugf("Runner[%v] read buf file %v error %v, ignore...", meta.RunnerName, meta.BufFile(), err)
			} else {
				log.Warnf("Runner[%v] read buf file %v error %v, ignore...", meta.RunnerName, meta.BufFile(), err)
			}
		} else {
			err = jsoniter.Unmarshal(buf, &cacheMap)
			if err != nil {
				log.Warnf("Runner[%v] Unmarshal read buf cache error %v, ignore...", meta.RunnerName, err)
			}
		}
		err = nil
	}

	return &Reader{
		meta:           meta,
		logPathPattern: logPathPattern,
		whence:         whence,
		expire:         expire,
		statInterval:   statInterval,
		maxOpenFiles:   maxOpenFiles,
		started:        false,
		startmux:       sync.Mutex{},
		status:         reader.StatusInit,
		fileReaders:    make(map[string]*ActiveReader), //armapmux
		cacheMap:       cacheMap,                       //armapmux
		armapmux:       sync.Mutex{},
		msgChan:        make(chan Result),
		errChan:        make(chan error),
		statsLock:      sync.RWMutex{},
	}, nil

}

//Expire 函数关闭过期的文件，再更新
func (mr *Reader) Expire() {
	var paths []string
	if atomic.LoadInt32(&mr.status) == reader.StatusStopped {
		return
	}
	mr.armapmux.Lock()
	defer mr.armapmux.Unlock()
	if atomic.LoadInt32(&mr.status) == reader.StatusStopped {
		return
	}
	for path, ar := range mr.fileReaders {
		if ar.expired(mr.expire) {
			ar.Close()
			delete(mr.fileReaders, path)
			delete(mr.cacheMap, path)
			mr.meta.RemoveSubMeta(path)
			paths = append(paths, path)
		}
	}
	if len(paths) > 0 {
		log.Infof("Runner[%v] expired logpath: %v", mr.meta.RunnerName, strings.Join(paths, ", "))
	}
}

func (mr *Reader) SetMode(mode string, value interface{}) (err error) {
	reg, err := reader.HeadPatternMode(mode, value)
	if err != nil {
		return fmt.Errorf("%v setmode error %v", mr.Name(), err)
	}
	if reg != nil {
		mr.headRegexp = reg
	}
	return
}

func (mr *Reader) sendError(err error) {
	if err == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("Reader %s Recovered from %v", mr.Name(), r)
		}
	}()
	mr.errChan <- err
}

func (mr *Reader) StatLogPath() {
	//达到最大打开文件数，不再追踪
	if len(mr.fileReaders) >= mr.maxOpenFiles {
		log.Warnf("Runner[%v] %v meet maxOpenFiles limit %v, ignore Stat new log...", mr.meta.RunnerName, mr.Name(), mr.maxOpenFiles)
		return
	}
	matches, err := filepath.Glob(mr.logPathPattern)
	if err != nil {
		log.Errorf("Runner[%v] stat logPathPattern error %v", mr.meta.RunnerName, err)
		mr.setStatsError("Runner[" + mr.meta.RunnerName + "] stat logPathPattern error " + err.Error())
		return
	}
	if len(matches) > 0 {
		log.Debugf("Runner[%v] StatLogPath %v find matches: %v", mr.meta.RunnerName, mr.logPathPattern, strings.Join(matches, ", "))
	}
	var newaddsPath []string
	for _, mc := range matches {
		rp, fi, err := GetRealPath(mc)
		if err != nil {
			log.Errorf("Runner[%v] file pattern %v match %v stat error %v, ignore this match...", mr.meta.RunnerName, mr.logPathPattern, mc, err)
			continue
		}
		if fi.IsDir() {
			log.Debugf("Runner[%v] %v is dir, mode[tailx] only support read file, ignore this match...", mr.meta.RunnerName, mc)
			continue
		}
		mr.armapmux.Lock()
		_, ok := mr.fileReaders[rp]
		mr.armapmux.Unlock()
		if ok {
			log.Debugf("Runner[%v] <%v> is collecting, ignore...", mr.meta.RunnerName, rp)
			continue
		}
		mr.armapmux.Lock()
		cacheline := mr.cacheMap[rp]
		mr.armapmux.Unlock()
		//过期的文件不追踪，除非之前追踪的并且有日志没读完
		if cacheline == "" && fi.ModTime().Add(mr.expire).Before(time.Now()) {
			log.Debugf("Runner[%v] <%v> is expired, ignore...", mr.meta.RunnerName, mc)
			continue
		}
		ar, err := NewActiveReader(mc, rp, mr.whence, mr.meta, mr.msgChan, mr.errChan)
		if err != nil {
			err = fmt.Errorf("runner[%v] NewActiveReader for matches %v error %v", mr.meta.RunnerName, rp, err)
			mr.sendError(err)
			log.Error(err, ", ignore this match...")
			continue
		}
		ar.readcache = cacheline
		if mr.headRegexp != nil {
			err = ar.br.SetMode(reader.ReadModeHeadPatternRegexp, mr.headRegexp)
			if err != nil {
				log.Errorf("Runner[%v] NewActiveReader for matches %v SetMode error %v", mr.meta.RunnerName, rp, err)
				mr.setStatsError("Runner[" + mr.meta.RunnerName + "] NewActiveReader for matches " + rp + " SetMode error " + err.Error())
			}
		}
		newaddsPath = append(newaddsPath, rp)
		mr.armapmux.Lock()
		if atomic.LoadInt32(&mr.status) != reader.StatusStopped {
			if err = mr.meta.AddSubMeta(rp, ar.br.Meta); err != nil {
				log.Errorf("Runner[%v] %v add submeta for %v err %v, but this reader will still working", mr.meta.RunnerName, mc, rp, err)
			}
			mr.fileReaders[rp] = ar
		} else {
			log.Warnf("Runner[%v] %v NewActiveReader but reader was stopped, ignore this...", mr.meta.RunnerName, mc)
		}
		mr.armapmux.Unlock()
		if atomic.LoadInt32(&mr.status) != reader.StatusStopped {
			go ar.Run()
		} else {
			log.Warnf("Runner[%v] %v NewActiveReader but reader was stopped, will not running...", mr.meta.RunnerName, mc)
		}
	}
	if len(newaddsPath) > 0 {
		log.Infof("Runner[%v] StatLogPath find new logpath: %v", mr.meta.RunnerName, strings.Join(newaddsPath, ", "))
	}
}

func (mr *Reader) getActiveReaders() []*ActiveReader {
	mr.armapmux.Lock()
	defer mr.armapmux.Unlock()
	var ars []*ActiveReader
	for _, ar := range mr.fileReaders {
		ars = append(ars, ar)
	}
	return ars
}

func (mr *Reader) Name() string {
	return "MultiReader:" + mr.logPathPattern
}

func (mr *Reader) Source() string {
	return mr.curFile
}

func (mr *Reader) setStatsError(err string) {
	mr.statsLock.Lock()
	defer mr.statsLock.Unlock()
	mr.stats.LastError = err
}

func (mr *Reader) Status() StatsInfo {
	mr.statsLock.RLock()
	defer mr.statsLock.RUnlock()

	ars := mr.getActiveReaders()
	for _, ar := range ars {
		st := ar.Status()
		if st.LastError != "" {
			mr.stats.LastError += "\n<" + ar.originpath + ">: " + st.LastError
		}
	}
	return mr.stats
}

func (mr *Reader) Close() (err error) {
	atomic.StoreInt32(&mr.status, reader.StatusStopped)
	// 停10ms为了管道中的数据传递完毕，确认reader run函数已经结束不会再读取，保证syncMeta的正确性
	time.Sleep(10 * time.Millisecond)
	mr.SyncMeta()
	ars := mr.getActiveReaders()
	var wg sync.WaitGroup
	for _, ar := range ars {
		wg.Add(1)
		go func(mar *ActiveReader) {
			defer wg.Done()
			xerr := mar.Close()
			if xerr != nil {
				log.Errorf("Runner[%v] Close ActiveReader %v error %v", mr.meta.RunnerName, mar.originpath, xerr)
			}
		}(ar)
	}
	wg.Wait()
	//在所有 active readers都关闭后再close msgChan
	close(mr.msgChan)
	close(mr.errChan)
	return
}

/*
	Start 仅调用一次，借用ReadLine启动，不能在new实例的时候启动，会有并发问题
	处理StatIntervel以及Expire两大循环任务
*/
func (mr *Reader) Start() {
	mr.startmux.Lock()
	defer mr.startmux.Unlock()
	if mr.started {
		return
	}
	go mr.run()
	mr.started = true
	log.Infof("%v MultiReader stat file deamon started", mr.Name())
}

func (mr *Reader) run() {
	for {
		if atomic.LoadInt32(&mr.status) == reader.StatusStopped {
			log.Warnf("%v stopped from running", mr.Name())
			return
		}
		mr.Expire()
		mr.StatLogPath()
		time.Sleep(mr.statInterval)
	}
}

func (mr *Reader) ReadLine() (data string, err error) {
	if !mr.started {
		mr.Start()
	}
	timer := time.NewTimer(time.Second)
	select {
	case result := <-mr.msgChan:
		mr.curFile = result.logpath
		data = result.result
	case err = <-mr.errChan:
	case <-timer.C:
	}
	timer.Stop()
	return
}

//SyncMeta 从队列取数据时同步队列，作用在于保证数据不重复。
func (mr *Reader) SyncMeta() {
	ars := mr.getActiveReaders()
	for _, ar := range ars {
		readcache := ar.SyncMeta()
		if readcache == "" {
			continue
		}
		mr.armapmux.Lock()
		mr.cacheMap[ar.realpath] = readcache
		mr.armapmux.Unlock()
	}
	mr.armapmux.Lock()
	buf, err := jsoniter.Marshal(mr.cacheMap)
	mr.armapmux.Unlock()
	if err != nil {
		log.Errorf("%v sync meta error %v, cacheMap %v", mr.Name(), err, mr.cacheMap)
		return
	}
	err = mr.meta.WriteBuf(buf, 0, 0, len(buf))
	if err != nil {
		log.Errorf("%v sync meta WriteBuf error %v, buf %v", mr.Name(), err, string(buf))
		return
	}
	return
}

func (mr *Reader) Lag() (rl *LagInfo, err error) {
	rl = &LagInfo{SizeUnit: "bytes"}
	var errStr string
	ars := mr.getActiveReaders()

	for _, ar := range ars {
		lg, subErr := ar.Lag()
		if subErr != nil {
			errStr += subErr.Error()
			log.Warn(subErr)
			continue
		}
		rl.Size += lg.Size
	}
	if len(errStr) > 0 {
		err = errors.New(errStr)
	}

	return rl, err
}

func (mr *Reader) Reset() (err error) {
	errMsg := make([]string, 0)
	if err = mr.meta.Reset(); err != nil {
		errMsg = append(errMsg, err.Error())
	}
	ars := mr.getActiveReaders()
	for _, ar := range ars {
		if ar.br != nil {
			if subErr := ar.br.Meta.Reset(); subErr != nil {
				errMsg = append(errMsg, subErr.Error())
			}
		}
	}
	if len(errMsg) != 0 {
		err = errors.New(strings.Join(errMsg, "\n"))
	}
	return
}
