package main

// 版本单源：VERSION 文件（续 v0.2.8 tag / beta v0.2.9 之后的老编号线）。
// 构建时注入：
//
//	go build -ldflags "-X main.appVersion=v0.3.0" .
//
// 本地用 build.ps1（自动读 VERSION + 写 Windows 文件属性）。
// 未注入时为 "dev"（go test / CI vet 走此分支）。

var appVersion = "dev"
