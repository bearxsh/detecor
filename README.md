### 编译
#### 第一次
> cd detector
> 
> go mod init detector
> 
> go mod tidy
> 
> go build main.go
#### 之后每次编译直接执行 go build main.go 命令就行了
### 运行
> nohup ./main > detector.log 2>&1 &
### 启动参数
-h 参看帮助

-p 设置启动端口（默认是8889）