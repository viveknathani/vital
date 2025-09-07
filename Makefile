build:
	go build -o vital main.go

run:
	make build && sudo ./vital