module tart-oven

go 1.24.3

require (
	github.com/pkg/sftp v1.13.10
	github.com/shirou/gopsutil/v3 v3.24.5
	golang.org/x/crypto v0.41.0
	golang.org/x/net v0.43.0
	golang.org/x/sys v0.35.0
)

require (
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/kr/fs v0.1.0 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/power-devops/perfstat v0.0.0-20210106213030-5aafc221ea8c // indirect
	github.com/shoenig/go-m1cpu v0.1.6 // indirect
	github.com/tklauser/go-sysconf v0.3.12 // indirect
	github.com/tklauser/numcpus v0.6.1 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
)

// gopsutil's go-m1cpu crashes at init on chips without the E-core IORegistry
// key (M5 Pro); see third_party/go-m1cpu.
replace github.com/shoenig/go-m1cpu => ./third_party/go-m1cpu
