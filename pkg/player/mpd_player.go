package player

import (
	"fmt"
	"go-musicfox/utils"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/fhs/gompd/v2/mpd"
)

var stateMapping = map[string]State{
	"play":  Playing,
	"pause": Paused,
	"stop":  Stopped,
}

func mpdErrorHandler(err error, ignore bool) {
	if err == nil {
		return
	}

	utils.Logger().Printf("err: %+v", err)
	if !ignore {
		panic(err)
	}
}

type mpdPlayer struct {
	bin        string
	configFile string
	network    string
	address    string

	watcher *mpd.Watcher
	l       sync.Mutex

	curMusic       UrlMusic
	curSongId      int
	timer          *utils.Timer
	latestPlayTime time.Time //避免切歌时产生的stop信号造成影响

	volume    int
	state     State
	timeChan  chan time.Duration
	stateChan chan State
	musicChan chan UrlMusic

	close chan struct{}
}

func NewMpdPlayer(bin, configFile, network, address string) Player {
	cmd := exec.Command(bin)
	if configFile != "" {
		cmd.Args = append(cmd.Args, configFile)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("MPD启动失败: %s, 详情:\n%s", err, output))
	}

	client, err := mpd.Dial(network, address)
	mpdErrorHandler(err, false)

	err = client.Stop()
	mpdErrorHandler(err, true)

	err = client.Single(true)
	mpdErrorHandler(err, true)

	watcher, err := mpd.NewWatcher(network, address, "", "player", "mixer")
	mpdErrorHandler(err, false)

	p := &mpdPlayer{
		bin:        bin,
		configFile: configFile,
		network:    network,
		address:    address,
		watcher:    watcher,
		timeChan:   make(chan time.Duration),
		stateChan:  make(chan State),
		musicChan:  make(chan UrlMusic),
		close:      make(chan struct{}),
	}

	go func() {
		defer utils.Recover(false)
		p.listen()
	}()

	go func() {
		defer utils.Recover(false)
		p.watch()
	}()

	p.SyncMpdStatus()
	return p
}

var _client *mpd.Client

func (p *mpdPlayer) client() *mpd.Client {
	var err error
	if _client != nil {
		if err = _client.Ping(); err == nil {
			return _client
		}
	}
	_client, err = mpd.Dial(p.network, p.address)
	mpdErrorHandler(err, false)
	return _client
}

func (p *mpdPlayer) SyncMpdStatus() {
	status, err := p.client().Status()
	mpdErrorHandler(err, true)

	p.volume, _ = strconv.Atoi(status["volume"])
	p.setState(stateMapping[status["state"]])
	duration, _ := time.ParseDuration(status["elapsed"] + "s")

	if p.timer != nil {
		p.timer.SetPassed(duration)
		select {
		case p.timeChan <- p.timer.Passed():
		default:
		}
	}
}

// listen 开始监听
func (p *mpdPlayer) listen() {
	var (
		err error
	)

	for {
		select {
		case <-p.close:
			return
		case p.curMusic = <-p.musicChan:
			p.latestPlayTime = time.Now()
			p.Paused()
			// 重置
			{
				if p.timer != nil {
					p.timer.Stop()
				}
				if p.curSongId != 0 {
					err = p.client().DeleteID(p.curSongId)
					mpdErrorHandler(err, true)
				}
			}

			p.curSongId, err = p.client().AddID(p.curMusic.Url, 0)
			mpdErrorHandler(err, false)

			// 计时器
			p.timer = utils.NewTimer(utils.Options{
				Duration:       8760 * time.Hour,
				TickerInternal: 200 * time.Millisecond,
				OnRun:          func(started bool) {},
				OnPaused:       func() {},
				OnDone:         func(stopped bool) {},
				OnTick: func() {
					select {
					case p.timeChan <- p.timer.Passed():
					default:
					}
				},
			})

			err = p.client().PlayID(p.curSongId)
			mpdErrorHandler(err, false)
			p.Resume()
		}
	}
}

func (p *mpdPlayer) watch() {
	for {
		select {
		case <-p.close:
			return
		case subSystem := <-p.watcher.Event:
			if subSystem == "mixer" {
				p.SyncMpdStatus()
				return
			}
			//避免切歌时产生的stop信号造成影响
			if subSystem == "player" && time.Now().Sub(p.latestPlayTime) >= time.Second*2 {
				p.SyncMpdStatus()
			}
		}
	}
}

func (p *mpdPlayer) setState(state State) {
	p.state = state
	select {
	case p.stateChan <- state:
	default:
	}
}

func (p *mpdPlayer) Play(songType SongType, url string, duration time.Duration) {
	music := UrlMusic{
		Url:      url,
		Type:     songType,
		Duration: duration,
	}
	select {
	case p.musicChan <- music:
	default:
	}
}

func (p *mpdPlayer) CurMusic() UrlMusic {
	return p.curMusic
}

func (p *mpdPlayer) Paused() {
	p.l.Lock()
	defer p.l.Unlock()
	if p.state != Playing {
		return
	}
	err := p.client().Pause(true)
	mpdErrorHandler(err, false)
	p.timer.Pause()
	p.setState(Paused)
}

func (p *mpdPlayer) Resume() {
	p.l.Lock()
	defer p.l.Unlock()
	if p.state == Playing {
		return
	}
	err := p.client().Pause(false)
	mpdErrorHandler(err, false)
	go p.timer.Run()
	p.setState(Playing)
}

func (p *mpdPlayer) Stop() {
	p.l.Lock()
	defer p.l.Unlock()
	if p.state == Stopped {
		return
	}
	err := p.client().Pause(true)
	mpdErrorHandler(err, false)
	p.timer.Pause()
	p.setState(Stopped)
}

func (p *mpdPlayer) Toggle() {
	switch p.State() {
	case Paused, Stopped:
		p.Resume()
	case Playing:
		p.Paused()
	}
}

func (p *mpdPlayer) Seek(duration time.Duration) {
	p.l.Lock()
	defer p.l.Unlock()
	err := p.client().SeekCur(duration, false)
	mpdErrorHandler(err, false)
	p.timer.SetPassed(duration)
}

func (p *mpdPlayer) PassedTime() time.Duration {
	if p.timer == nil {
		return 0
	}
	return p.timer.Passed()
}

func (p *mpdPlayer) TimeChan() <-chan time.Duration {
	return p.timeChan
}

func (p *mpdPlayer) State() State {
	return p.state
}

func (p *mpdPlayer) StateChan() <-chan State {
	return p.stateChan
}

func (p *mpdPlayer) UpVolume() {
	p.l.Lock()
	defer p.l.Unlock()
	if p.volume+5 >= 100 {
		p.volume = 100
	} else {
		p.volume += 5
	}
	_ = p.client().SetVolume(p.volume)
}

func (p *mpdPlayer) DownVolume() {
	p.l.Lock()
	defer p.l.Unlock()
	if p.volume-5 <= 0 {
		p.volume = 0
	} else {
		p.volume -= 5
	}
	_ = p.client().SetVolume(p.volume)
}

func (p *mpdPlayer) Close() {
	if p.timer != nil {
		p.timer.Stop()
	}
	p.close <- struct{}{}

	err := p.watcher.Close()
	mpdErrorHandler(err, true)

	err = p.client().Stop()
	mpdErrorHandler(err, true)

	err = p.client().Close()
	mpdErrorHandler(err, true)

	cmd := exec.Command(p.bin)
	if p.configFile != "" {
		cmd.Args = append(cmd.Args, p.configFile)
	}
	cmd.Args = append(cmd.Args, "--kill")
	_ = cmd.Run()
}
