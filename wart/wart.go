package wart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/robertkrimen/otto"
	_ "github.com/robertkrimen/otto/underscore"
	"github.com/robfig/cron/v3"
	log "github.com/sirupsen/logrus"
)

//Status Constants
const DISABLED = "disabled"
const CRASHED = "crashed"
const ONLINE = "online"
const ENABLED = "enabled"
const STOPPED = "stopped"
const RUNNING = "running"

var ctx = context.Background()

type Wart struct {
	RedisAddr       string
	RedisPassword   string
	Cluster         string
	WartName        string
	ScriptList      string
	Client          *redis.Client
	Healthy         bool
	ThreadCount     int
	threads         map[string]*ThreadMeta
	jobs            map[string]*JobMeta
	SecondsTillDead int
	VMStopChan      chan func()
	shuttingDown    bool
}

type TaskInterface interface {
	getVM() *otto.Otto
}

func Create(configFile string, redisAddr string, redisPassword string, cluster string, wartName string, scriptList string, host bool, hostPort string, healthPort string) (*Wart, error) {
	if configFile != "" {
		fBytes, err := ioutil.ReadFile(configFile)
		if err == nil {
			var f interface{}
			err2 := json.Unmarshal(fBytes, &f)
			if err2 == nil {
				m := f.(map[string]interface{})
				redisAddr = m["redis-address"].(string)
				redisPassword = m["redis-password"].(string)
				cluster = m["cluster"].(string)
				wartName = m["name"].(string)
				host = m["host"].(bool)
			}
		}
	}

	if len(wartName) == 0 {
		wartName = generateRandomName(10)
	}
	w := &Wart{RedisAddr: redisAddr, RedisPassword: redisPassword,
		Cluster: cluster, WartName: wartName, ScriptList: scriptList,
		Healthy: true, SecondsTillDead: 1}

	if w.RedisAddr == "" {
		return nil, errors.New("no redis address provided")
	}

	w.Client = redis.NewClient(&redis.Options{
		Addr:     w.RedisAddr,
		Password: w.RedisPassword, // no password set
		DB:       0,               // use default DB
	})

	pong, pongErr := w.Client.Ping(ctx).Result()

	if pongErr != nil && pong != "PONG" {
		return nil, errors.New("redis failed ping")
	}

	w.Client.HSet(ctx, w.Cluster+":Warts:"+w.WartName, "State", ONLINE)
	w.Client.HSet(ctx, w.Cluster+":Warts:"+w.WartName, "Status", ENABLED)

	if w.ScriptList != "" {
		err := loadScripts(w, w.ScriptList)
		if err != nil {
			return nil, err
		}
	}

	if host {
		http.HandleFunc("/", w.handleEndpoint)
		go func() { http.ListenAndServe(":"+hostPort, nil) }()
	}

	// create `ServerMux`
	mux := http.NewServeMux()

	// create a default route handler
	mux.HandleFunc("/", func(res http.ResponseWriter, req *http.Request) {
		pong, pongErr := w.Client.Ping(ctx).Result()

		if pongErr != nil && pong != "PONG" {
			http.Error(res, "Unhealthy", 500)
		} else {
			fmt.Fprint(res, "{}")
		}
	})

	// create new server
	healthServer := http.Server{
		Addr:    fmt.Sprintf(":%v", healthPort), // :{port}
		Handler: mux,
	}
	go func() { healthServer.ListenAndServe() }()

	return w, nil
}

func generateRandomName(length int) (out string) {
	chars := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"
	for i := 0; i < length; i++ {
		out += string(chars[rand.Intn(len(chars))])
	}

	return
}

func (w *Wart) Shutdown() {
	w.shuttingDown = true
	threads := getThreads(w)
	for i := range threads {
		threads[i].stop(w)
	}
}

func getThreads(w *Wart) map[string]*ThreadMeta {
	keys := w.Client.Keys(ctx, w.Cluster+":Threads:*").Val()
	if w.threads == nil {
		w.threads = make(map[string]*ThreadMeta, 0)
	}

	for i := range keys {
		if w.threads[keys[i]] == nil {
			w.threads[keys[i]] = &ThreadMeta{Key: keys[i], Stopped: true}
		}
	}
	return w.threads
}

func getJobs(w *Wart) map[string]*JobMeta {
	keys := w.Client.Keys(ctx, w.Cluster+":Jobs:*").Val()
	if w.jobs == nil {
		w.jobs = make(map[string]*JobMeta, 0)
	}

	for i := range keys {
		if w.jobs[keys[i]] == nil {
			w.jobs[keys[i]] = &JobMeta{Key: keys[i], Stopped: true}
		}
	}
	return w.jobs
}

func IsEnabled(w *Wart) bool {
	status := w.Client.HGet(ctx, w.Cluster+":Warts:"+w.WartName, "Status").Val()
	if w.shuttingDown || status == DISABLED {
		return false
	}
	return true
}

func CheckThreads(w *Wart) {
	threads := getThreads(w)
	for i := range threads {
		threadStatus := threads[i].getStatus(w)
		threadState := threads[i].getState(w)
		if threadStatus != DISABLED {
			if threadState == STOPPED {
				threads[i].take(w)
				continue
			}
			//Check to see if thread is hung or fell over before its state was updated
			lastHeartbeat, err := threads[i].getHeartBeat(w)

			if err == nil {
				deadSeconds, err := threads[i].getDeadSeconds(w)
				if err == nil {
					if deadSeconds == 0 {
						deadSeconds = w.SecondsTillDead
					}
					elapsed := time.Since(time.Unix(0, int64(lastHeartbeat)))
					if int(elapsed.Seconds()) > deadSeconds && lastHeartbeat != 0 {
						threads[i].take(w)
					}
				} else {
					log.WithError(err).Error("Error getting dead seconds")
				}
			} else {
				log.WithError(err).Error("Error checking thread hang")
			}
		}
	}
}

func CheckJobs(w *Wart) {
	jobs := getJobs(w)
	for i := range jobs {
		jobStatus := jobs[i].getStatus(w)
		jobState := jobs[i].getState(w)
		if jobStatus != DISABLED {
			if jobState == STOPPED {
				jobs[i].schedule(w)
				continue
			}
		}
	}
}

func loadScripts(w *Wart, scripts string) error {
	scriptArray := strings.Split(scripts, ",")
	for i := range scriptArray {
		scriptName := scriptArray[i]
		log.Info("Loading", scriptName)
		fBytes, err := ioutil.ReadFile(scriptName)
		if err != nil {
			return err
		}
		key := w.Cluster + ":Threads:" + scriptName
		w.Client.HSet(ctx, key, "Source", string(fBytes))
		w.Client.HSet(ctx, key, "Status", ENABLED)
		w.Client.HSet(ctx, key, "State", STOPPED)
		w.Client.HSet(ctx, key, "Heartbeat", 0)
		w.Client.HSet(ctx, key, "Hang", 1)
		w.Client.HSet(ctx, key, "DeadSeconds", 2)
		w.Client.HSet(ctx, key, "Owner", "")
		w.Client.HSet(ctx, key, "Error", "")
		w.Client.HSet(ctx, key, "ErrorTime", "")
	}

	return nil
}

func (wart *Wart) handleEndpoint(w http.ResponseWriter, r *http.Request) {
	if wart.Healthy {
		em := getEndpoint(wart, html.EscapeString(r.URL.Path))
		if em != nil {
			em.run(wart, w, r)
		} else {
			http.Error(w, "Endpoint not found", http.StatusNotFound)
		}
	}
}

func applyLibrary(w *Wart, tm TaskInterface) {
	tm.getVM().Set("redis", map[string]interface{}{
		"Do2": w.Client.Do,
		"Do": func(call otto.FunctionCall) otto.Value {
			arguments := make([]interface{}, 0)

			for i := range call.ArgumentList {
				a, _ := call.Argument(i).ToString()
				arguments = append(arguments, a)
			}
			v := w.Client.Do(ctx, arguments...)
			value, _ := tm.getVM().ToValue(v.Val())
			return value
		},
		"Blpop": func(call otto.FunctionCall) otto.Value {
			timeout, err := call.Argument(0).ToInteger()
			rKey := call.Argument(1).String()
			if err == nil {
				item := w.Client.BLPop(ctx, time.Duration(timeout)*time.Second, rKey)
				if len(item.Val()) > 0 {
					value, _ := tm.getVM().ToValue(item.Val()[1])
					return value
				}
			}
			value, _ := tm.getVM().ToValue("")
			return value
		},
	})

	tm.getVM().Set("http", map[string]interface{}{
		"Get":      httpGet,
		"Post":     httpPost,
		"PostForm": httpPostForm,
		"Put":      httpPut,
		"Delete":   httpDelete,
	})

	tm.getVM().Set("sql", map[string]interface{}{
		"New": newSQLWrapper,
	})

	tm.getVM().Set("wart", map[string]interface{}{
		"Name":         w.WartName,
		"Cluster":      w.Cluster,
		"ShuttingDown": w.Shutdown,
	})

	tm.getVM().Set("env", map[string]interface{}{
		"Get": func(call otto.FunctionCall) otto.Value {
			envName, _ := call.Argument(0).ToString()
			if envName == "undefined" {
				return otto.New().MakeSyntaxError("Missing parameter")
			}
			envValue, exists := os.LookupEnv(envName)
			var value otto.Value
			if exists {
				value, _ = tm.getVM().ToValue(envValue)
			} else {
				value = otto.UndefinedValue()
			}

			return value
		},
		"Set": func(call otto.FunctionCall) otto.Value {
			envName, _ := call.Argument(0).ToString()
			envValue, _ := call.Argument(1).ToString()
			if envName == "undefined" || envValue == "undefined" {
				return otto.New().MakeSyntaxError("Missing parameter")
			}

			err := os.Setenv(envName, envValue)
			if err != nil {
				return otto.New().MakeSyntaxError("Error setting env")
			}

			return otto.NullValue()
		},
		"Unset": func(call otto.FunctionCall) otto.Value {
			envName, _ := call.Argument(0).ToString()
			if envName == "undefined" {
				return otto.New().MakeSyntaxError("Missing parameter")
			}

			err := os.Unsetenv(envName)
			if err != nil {
				return otto.New().MakeSyntaxError("Error setting env")
			}

			return otto.NullValue()
		},
	})

	switch tm.(type) {
	case *JobMeta:
		t := tm.(*JobMeta)
		t.vm.Set("job", map[string]interface{}{
			"Key":     t.Key,
			"Stopped": t.Stopped,
			"State": func() otto.Value {
				value, _ := t.vm.ToValue(t.getState(w))
				return value
			},
			"Status": func() otto.Value {
				value, _ := t.vm.ToValue(t.getStatus(w))
				return value
			},
			"Disable": func() {
				t.disable(w)
			},
		})
	case *ThreadMeta:
		t := tm.(*ThreadMeta)
		tm.getVM().Set("thread", map[string]interface{}{
			"Key":     t.Key,
			"Stopped": t.Stopped,
			"State": func() otto.Value {
				value, _ := t.vm.ToValue(t.getState(w))
				return value
			},
			"Status": func() otto.Value {
				value, _ := t.vm.ToValue(t.getStatus(w))
				return value
			},
			"Disable": func() {
				t.disable(w)
			},
			"Stop": func() {
				t.stop(w)
			},
		})
	}

}

func newWithSeconds() *cron.Cron {
	return cron.New(cron.WithParser(cron.NewParser(cron.Second|cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.DowOptional|cron.Descriptor)), cron.WithChain())
}
