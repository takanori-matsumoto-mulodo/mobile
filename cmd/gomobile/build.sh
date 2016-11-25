#!/bin/bash


#git clone https://go.googlesource.com/mobile
go build main.go binary_xml.go bind.go bind_androidapp.go bind_iosapp.go build.go build_androidapp.go build_iosapp.go cert.go clean.go dex.go doc.go env.go hashes.go init.go install.go manifest.go strings_flag.go version.go writer.go
mv main $GOPATH/bin/gomobile

