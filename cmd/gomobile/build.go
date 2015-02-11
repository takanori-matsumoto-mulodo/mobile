// Copyright 2015 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

var ctx = build.Default
var pkg *build.Package

var cmdBuild = &command{
	run:   runBuild,
	Name:  "build",
	Usage: "[package]",
	Short: "compile android APK and/or iOS app",
	Long: `
Build compiles and encodes the app named by the import path.

The named package must define a main function.

If an AndroidManifest.xml is defined in the package directory, it is
added to the APK file. Otherwise, a default manifest is generated.

If the package directory contains an assets subdirectory, its contents
are copied into the APK file.

The -o flag specifies the output file name. If not specified, the
output file name depends on the package built. The output file must end
in '.apk'.

The -v flag provides verbose output, including the list of packages built.

These build flags are shared by the build, install, and test commands.
For documentation, see 'go help build':
	-a
	-i
	-tags 'tag list'
`,
}

// TODO: -n
// TODO: -x
// TODO: -mobile

func runBuild(cmd *command) error {
	cwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	args := cmd.flag.Args()

	switch len(args) {
	case 0:
		pkg, err = ctx.ImportDir(cwd, build.ImportComment)
	case 1:
		pkg, err = ctx.Import(args[0], cwd, build.ImportComment)
	default:
		cmd.usage()
		os.Exit(1)
	}
	if err != nil {
		return err
	}

	// Check that we are compiling an app.
	if pkg.Name != "main" {
		return fmt.Errorf(`package %q: can only build package "main"`, pkg.Name)
	}
	importsApp := false
	for _, path := range pkg.Imports {
		if path == "golang.org/x/mobile/app" {
			importsApp = true
			break
		}
	}
	if !importsApp {
		return fmt.Errorf(`%s does not import "golang.org/x/mobile/app"`, pkg.ImportPath)
	}

	workPath, err := ioutil.TempDir("", "gobuildapk-work-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workPath)

	libName := path.Base(pkg.ImportPath)
	manifestData, err := ioutil.ReadFile(filepath.Join(pkg.Dir, "AndroidManifest.xml"))
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		buf := new(bytes.Buffer)
		buf.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
		err := manifestTmpl.Execute(buf, manifestTmplData{
			// TODO(crawshaw): a better package path.
			JavaPkgPath: "org.golang.todo." + pkg.Name,
			Name:        strings.ToUpper(pkg.Name[:1]) + pkg.Name[1:],
			LibName:     libName,
		})
		if err != nil {
			return err
		}
		manifestData = buf.Bytes()
		if buildV {
			fmt.Fprintf(os.Stderr, "generated AndroidManifest.xml:\n%s\n", manifestData)
		}
	} else {
		libName, err = manifestLibName(manifestData)
		if err != nil {
			return err
		}
	}
	libPath := filepath.Join(workPath, "lib"+libName+".so")

	gopath := goEnv("GOPATH")
	var ccpath string
	for _, p := range filepath.SplitList(gopath) {
		ccpath = filepath.Join(p, filepath.FromSlash("pkg/gomobile/android-"+ndkVersion+"/arm/bin"))
		if _, err = os.Stat(ccpath); err == nil {
			break
		}
	}

	if err != nil || ccpath == "" {
		// TODO(crawshaw): call gomobile init
		return fmt.Errorf("android %s toolchain not installed in $GOPATH/pkg/gomobile, run gomobile init", ndkVersion)
	}

	gocmd := exec.Command(
		`go`,
		`build`,
		`-ldflags="-shared"`,
		`-tags=`+strconv.Quote(strings.Join(ctx.BuildTags, ",")),
		`-o`, libPath)
	if buildV {
		gocmd.Args = append(gocmd.Args, "-v")
	}
	if buildI {
		gocmd.Args = append(gocmd.Args, "-i")
	}

	gocmd.Args = append(gocmd.Args, pkg.ImportPath)

	gocmd.Stdout = os.Stdout
	gocmd.Stderr = os.Stderr
	gocmd.Env = []string{
		`GOOS=android`,
		`GOARCH=arm`,
		`GOARM=7`,
		`CGO_ENABLED=1`,
		`CC=` + filepath.Join(ccpath, "arm-linux-androideabi-gcc"),
		`CXX=` + filepath.Join(ccpath, "arm-linux-androideabi-g++"),
		`GOGCCFLAGS="-fPIC -marm -pthread -fmessage-length=0"`,
		`GOROOT=` + goEnv("GOROOT"),
		`GOPATH=` + gopath,
	}
	if err := gocmd.Run(); err != nil {
		return err
	}

	block, _ := pem.Decode([]byte(debugCert))
	if block == nil {
		return errors.New("no debug cert")
	}
	privKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return err
	}

	if *buildO == "" {
		*buildO = filepath.Base(pkg.Dir) + ".apk"
	}
	if !strings.HasSuffix(*buildO, ".apk") {
		return fmt.Errorf("output file name %q does not end in '.apk'", *buildO)
	}
	out, err := os.Create(*buildO)
	if err != nil {
		return err
	}

	apkw := NewWriter(out, privKey)

	w, err := apkw.Create("AndroidManifest.xml")
	if err != nil {
		return err
	}
	if _, err := w.Write(manifestData); err != nil {
		return err
	}

	r, err := os.Open(libPath)
	if err != nil {
		return err
	}
	w, err = apkw.Create("lib/armeabi/lib" + libName + ".so")
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		return err
	}

	// Add any assets.
	assetsDir := filepath.Join(pkg.Dir, "assets")
	assetsDirExists := true
	fi, err := os.Stat(assetsDir)
	if err != nil {
		if os.IsNotExist(err) {
			assetsDirExists = false
		} else {
			return err
		}
	} else {
		assetsDirExists = fi.IsDir()
	}
	if assetsDirExists {
		filepath.Walk(assetsDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			name := "assets/" + path[len(assetsDir)+1:]
			w, err := apkw.Create(name)
			if err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(w, f)
			return err
		})
	}

	// TODO: add gdbserver to apk?

	return apkw.Close()
}

// "Build flags", used by multiple commands.
var (
	buildA bool    // -a
	buildV bool    // -v
	buildI bool    // -i
	buildO *string // -o
)

func addBuildFlags(cmd *command) {
	cmd.flag.BoolVar(&buildA, "a", false, "")
	cmd.flag.BoolVar(&buildI, "i", false, "")
	cmd.flag.Var((*stringsFlag)(&ctx.BuildTags), "tags", "")
}

func addBuildFlagsNXV(cmd *command) {
	// TODO: -n, -x
	cmd.flag.BoolVar(&buildV, "v", false, "")
}

func init() {
	buildO = cmdBuild.flag.String("o", "", "output file")
	addBuildFlags(cmdBuild)
	addBuildFlagsNXV(cmdBuild)

	addBuildFlags(cmdInstall)
	addBuildFlagsNXV(cmdInstall)

	addBuildFlagsNXV(cmdInit)
}

// A random uninteresting private key.
// Must be consistent across builds so newer app versions can be installed.
const debugCert = `
-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAy6ItnWZJ8DpX9R5FdWbS9Kr1U8Z7mKgqNByGU7No99JUnmyu
NQ6Uy6Nj0Gz3o3c0BXESECblOC13WdzjsH1Pi7/L9QV8jXOXX8cvkG5SJAyj6hcO
LOapjDiN89NXjXtyv206JWYvRtpexyVrmHJgRAw3fiFI+m4g4Qop1CxcIF/EgYh7
rYrqh4wbCM1OGaCleQWaOCXxZGm+J5YNKQcWpjZRrDrb35IZmlT0bK46CXUKvCqK
x7YXHgfhC8ZsXCtsScKJVHs7gEsNxz7A0XoibFw6DoxtjKzUCktnT0w3wxdY7OTj
9AR8mobFlM9W3yirX8TtwekWhDNTYEu8dwwykwIDAQABAoIBAA2hjpIhvcNR9H9Z
BmdEecydAQ0ZlT5zy1dvrWI++UDVmIp+Ve8BSd6T0mOqV61elmHi3sWsBN4M1Rdz
3N38lW2SajG9q0fAvBpSOBHgAKmfGv3Ziz5gNmtHgeEXfZ3f7J95zVGhlHqWtY95
JsmuplkHxFMyITN6WcMWrhQg4A3enKLhJLlaGLJf9PeBrvVxHR1/txrfENd2iJBH
FmxVGILL09fIIktJvoScbzVOneeWXj5vJGzWVhB17DHBbANGvVPdD5f+k/s5aooh
hWAy/yLKocr294C4J+gkO5h2zjjjSGcmVHfrhlXQoEPX+iW1TGoF8BMtl4Llc+jw
lKWKfpECgYEA9C428Z6CvAn+KJ2yhbAtuRo41kkOVoiQPtlPeRYs91Pq4+NBlfKO
2nWLkyavVrLx4YQeCeaEU2Xoieo9msfLZGTVxgRlztylOUR+zz2FzDBYGicuUD3s
EqC0Wv7tiX6dumpWyOcVVLmR9aKlOUzA9xemzIsWUwL3PpyONhKSq7kCgYEA1X2F
f2jKjoOVzglhtuX4/SP9GxS4gRf9rOQ1Q8DzZhyH2LZ6Dnb1uEQvGhiqJTU8CXxb
7odI0fgyNXq425Nlxc1Tu0G38TtJhwrx7HWHuFcbI/QpRtDYLWil8Zr7Q3BT9rdh
moo4m937hLMvqOG9pyIbyjOEPK2WBCtKW5yabqsCgYEAu9DkUBr1Qf+Jr+IEU9I8
iRkDSMeusJ6gHMd32pJVCfRRQvIlG1oTyTMKpafmzBAd/rFpjYHynFdRcutqcShm
aJUq3QG68U9EAvWNeIhA5tr0mUEz3WKTt4xGzYsyWES8u4tZr3QXMzD9dOuinJ1N
+4EEumXtSPKKDG3M8Qh+KnkCgYBUEVSTYmF5EynXc2xOCGsuy5AsrNEmzJqxDUBI
SN/P0uZPmTOhJIkIIZlmrlW5xye4GIde+1jajeC/nG7U0EsgRAV31J4pWQ5QJigz
0+g419wxIUFryGuIHhBSfpP472+w1G+T2mAGSLh1fdYDq7jx6oWE7xpghn5vb9id
EKLjdwKBgBtz9mzbzutIfAW0Y8F23T60nKvQ0gibE92rnUbjPnw8HjL3AZLU05N+
cSL5bhq0N5XHK77sscxW9vXjG0LJMXmFZPp9F6aV6ejkMIXyJ/Yz/EqeaJFwilTq
Mc6xR47qkdzu0dQ1aPm4XD7AWDtIvPo/GG2DKOucLBbQc2cOWtKS
-----END RSA PRIVATE KEY-----
`