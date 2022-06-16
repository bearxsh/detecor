package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	log "github.com/sirupsen/logrus"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"net"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"sync"
	"time"
)

type DetectTask struct {
	Id          int
	TaskId      string
	Name        string
	Action      string `gorm:"-"`
	Type        string
	Param       string
	Interval    int
	Status      int
	ReportAddr  string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TableName 指定表名
func (task *DetectTask) TableName() string {
	return "detect_task"
}

var (
	db *gorm.DB
	// 缓存 避免频繁查询数据库
	taskChannelMap sync.Map
	help           bool
	bindPort       int
)

func init() {
	// 初始化命令行解析
	flag.BoolVar(&help, "h", false, "查看帮助")
	flag.IntVar(&bindPort, "p", 8889, "设置启动端口")

	flag.Parse()
	// 初始化日志
	log.SetReportCaller(true)
	log.SetFormatter(&log.TextFormatter{
		// 注意：2006-01-02 15:04:05.000 是固定的，不能改动！
		TimestampFormat: "2006-01-02 15:04:05.000",
		FullTimestamp:   true,
		CallerPrettyfier: func(frame *runtime.Frame) (function string, file string) {
			// 精简文件名
			fileName := path.Base(frame.File)
			return frame.Function, fileName + ":" + strconv.Itoa(frame.Line)
		},
	})
	// 初始化数据库
	var err error
	db, err = gorm.Open(sqlite.Open("detector.db"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		panic(err)
	}
	sqlStmt := `CREATE TABLE IF NOT EXISTS "detect_task" (
					"id" INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
					"task_id" TEXT NOT NULL,
					"name" TEXT NOT NULL,
					"type" TEXT NOT NULL,
					"param" TEXT NOT NULL,
					"interval" INTEGER NOT NULL,
					"status" INTEGER NOT NULL,
					"report_addr" TEXT NOT NULL,
					"description" TEXT NOT NULL,
					"created_at" datetime NOT NULL,
					"updated_at" datetime
				);`
	if err = db.Exec(sqlStmt).Error; err != nil {
		panic(err)
	}
	createIndexStmt := `CREATE UNIQUE INDEX IF NOT EXISTS "unique_index_task_id" ON "detect_task" ("task_id" ASC)`
	if err = db.Exec(createIndexStmt).Error; err != nil {
		panic(err)
	}
}

func doNetUpDown(param string) string {
	command := fmt.Sprintf("ethtool %s | awk 'END{print $3}'", param)
	output, err := exec.Command("/bin/sh", "-c", command).Output()
	if err != nil {
		log.Errorf("Failed to exec command [%s]: %s", command, err)
		return "unknown"
	}
	result := string(output)
	rs := result[:len(result)-1]
	if rs != "yes" && rs != "no" {
		rs = "unknown"
	}
	return rs
}

func doHttping(param string) string {
	var latency string
	command := fmt.Sprintf("httping -c 5 -t 1 %s | awk 'END{print $4}' | awk -F/ '{print $2}'", param)
	output, err := exec.Command("/bin/sh", "-c", command).Output()
	if err != nil {
		log.Errorf("Failed to exec command [%s]: %s", command, err)
		latency = "0"
	} else {
		result := string(output)
		rs := result[:len(result)-1]
		if rs == "" {
			latency = "0"
		} else {
			latency = rs
		}
	}
	return latency
}
func doTcp(param string) string {
	conn, err := net.DialTimeout("tcp", param, 2*time.Second)
	if err != nil {
		//log.Errorf("Failed to establish connection with [%s]: %s", param, err)
		return "false"
	}
	conn.Close()
	return "true"
}

func doPing(param string) string {
	// 判断不通的ip会阻塞很久，需要设置设置超时时间，有的系统是-t，有的系统是-W
	system := runtime.GOOS
	var command string
	if system == "linux" {
		command = fmt.Sprintf("ping -c 1 -W 1 %s > /dev/null && echo true || echo false", param)
	} else {
		command = fmt.Sprintf("ping -c 1 -t 1 %s > /dev/null && echo true || echo false", param)
	}
	// 注意：Output末尾会有换行符 \n
	output, err := exec.Command("/bin/sh", "-c", command).Output()
	if err != nil {
		log.Errorf("Failed to exec command [%s]: %s", command, err)
		return "unknown"
	}
	result := string(output)
	return result[:len(result)-1]
}

func handleConn(conn net.Conn) {
	defer func(conn net.Conn) {
		log.Infof("Close connection, remoteAddr is [%s].", conn.RemoteAddr())
		err := conn.Close()
		if err != nil {
			log.Errorf("Failed to close connection: %s", err)
		}
	}(conn)
	var e error
	// 本业务用的是短连接，这里可以不用for循环
LOOP:
	for {
		// 必须确保buf容量足够
		buf := make([]byte, 4*1024)
		if e = conn.SetReadDeadline(time.Now().Add(60 * time.Second)); e != nil {
			log.Errorf("Failed to set readDeadLine: %s", e)
			break
		}
		var n int
		n, e = conn.Read(buf)
		if e != nil {
			log.Errorf("Read [%d] bytes: %s", n, e)
			break
		}
		log.Infof("Read [%d] bytes, content is [%s].", n, string(buf[:n]))
		task := new(DetectTask)
		if e = json.Unmarshal(buf[:n], task); e != nil {
			log.Errorf("Failed to decode JSON, content is [%s]: [%s]", string(buf[:n]), e)
			break
		}
		// TODO 参数校验
		switch task.Action {
		case "create":
			{
				task.Status = 1
				e = db.Create(task).Error
				if e != nil {
					log.Errorf("Failed to insert: %s", e)
					break LOOP
				}
				go detect(task)
				break LOOP
			}
		case "stop":
			{
				db.Model(&DetectTask{}).Where("task_id=?", task.TaskId).Update("status", 0)
				value, exist := taskChannelMap.Load(task.TaskId)
				if exist {
					taskChannelMap.Delete(task.TaskId)
					ch, _ := value.(chan struct{})
					ch <- struct{}{}
				} else {
					log.Errorf("The task [%s] does not exist or is not running", task.TaskId)
					e = errors.New("the task does not exist or is not running")
				}
				break LOOP
			}
		case "start":
			{
				_, exist := taskChannelMap.Load(task.TaskId)
				if exist {
					log.Errorf("The task [%s] is already running", task.TaskId)
					e = errors.New("the task is already running")
					break LOOP
				}
				var dt DetectTask
				e = db.Where("task_id=?", task.TaskId).First(&dt).Error
				if errors.Is(e, gorm.ErrRecordNotFound) {
					log.Errorf("The task [%s] does not exist", task.TaskId)
					e = errors.New("the task does not exist")
					break LOOP
				}
				db.Model(&DetectTask{}).Where("task_id=?", task.TaskId).Update("status", 1)
				go detect(&dt)
				break LOOP
			}
		case "delete":
			{
				var dt DetectTask
				e = db.Where("task_id=?", task.TaskId).First(&dt).Error
				if errors.Is(e, gorm.ErrRecordNotFound) {
					log.Errorf("The task [%s] does not exist", task.TaskId)
					e = errors.New("the task does not exist")
					break LOOP
				}
				if dt.Status == 1 {
					value, exist := taskChannelMap.Load(task.TaskId)
					if exist {
						taskChannelMap.Delete(task.TaskId)
						ch, _ := value.(chan struct{})
						ch <- struct{}{}
					} else {
						log.Warnf("The task [%s] should be running, but it is not.", task.TaskId)
					}
				}
				db.Where("task_id=?", task.TaskId).Delete(&DetectTask{})
				break LOOP
			}

		case "update":
			{
				var dt DetectTask
				e = db.Where("task_id=?", task.TaskId).First(&dt).Error
				if errors.Is(e, gorm.ErrRecordNotFound) {
					log.Errorf("The task [%s] does not exist", task.TaskId)
					e = errors.New("the task does not exist")
					break LOOP
				}
				if dt.Status == 1 {
					value, exist := taskChannelMap.Load(task.TaskId)
					if exist {
						taskChannelMap.Delete(task.TaskId)
						ch, _ := value.(chan struct{})
						ch <- struct{}{}
					} else {
						log.Warnf("The task [%s] should be running, but it is not.", task.TaskId)
					}
					go detect(task)
				}
				db.Model(&DetectTask{}).Where("task_id=?", task.TaskId).Select("Name", "Type", "Param", "Interval", "ReportAddr", "Description").Updates(task)
				break LOOP
			}
		default:
			{
				log.Errorf("Unknown action: [%s]", task.Action)
				e = errors.New("unknown action")
				break LOOP
			}
		}
	}
	code := 200
	msg := "ok"
	if e != nil {
		code = 301
		msg = e.Error()
	}
	response := fmt.Sprintf("<set_res><code>%d</code><msg>%s</msg></set_res>", code, msg)
	_, e = conn.Write([]byte(response))
	if e != nil {
		log.Errorf("Failed to write: %s", e)
	}

}

func detect(task *DetectTask) {
	quit := make(chan struct{})
	taskChannelMap.Store(task.TaskId, quit)
	ticker := time.NewTicker(time.Duration(task.Interval) * time.Second)
	defer ticker.Stop()
	for {
		var rs string
		if task.Type == "ping" {
			rs = doPing(task.Param)
		} else if task.Type == "httping" {
			rs = doHttping(task.Param)
		} else if task.Type == "netupdown" {
			rs = doNetUpDown(task.Param)
		} else if task.Type == "tcp" {
			rs = doTcp(task.Param)
		} else {
			log.Errorf("The task [%s], unknown type [%s]", task.TaskId, task.Type)
			continue
		}
		log.Infof("The task [%s] %s [%s] result [%s]", task.TaskId, task.Type, task.Param, rs)
		response := fmt.Sprintf("<con_res><id>%s</id><type>%s</type><status>%s</status></con_res>", task.TaskId, task.Type, rs)
		// 返回处理结果
		client, err := net.DialTimeout("tcp", task.ReportAddr, 2*time.Second)
		if err != nil {
			log.Errorf("Failed to establish connection with [%s]: %s", task.ReportAddr, err)
		} else {
			_, _ = client.Write([]byte(response))
			_ = client.Close()
		}
		select {
		case <-quit:
			log.Infof("The task [%s] exit", task.TaskId)
			return
		case <-ticker.C:
		}
	}
}

//  重启时恢复正在执行的任务
func loadTaskOnStart() {
	var tasks []DetectTask
	if err := db.Where("status=?", 1).Find(&tasks).Error; err != nil {
		panic(err)
	}
	log.Infof("[%d] task need to run", len(tasks))
	for i := range tasks {
		go detect(&tasks[i])
	}
}
func handleCommand() {
	if help {
		flag.Usage()
		os.Exit(0)
	}
}
func main() {
	handleCommand()
	loadTaskOnStart()
	// 启动网络服务
	listen, err := net.Listen("tcp", fmt.Sprintf(":%d", bindPort))
	if err != nil {
		panic(err)
	}
	log.Infof("The server starts successfully, listening on port [%d].", bindPort)
	for {
		accept, err := listen.Accept()
		if err != nil {
			log.Errorf("Failed to establish connection: %s", err)
			break
		}
		log.Infof("Connection establishes successfully, remoteAddr is [%s].", accept.RemoteAddr())
		go handleConn(accept)
	}
}
