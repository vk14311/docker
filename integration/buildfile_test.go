package docker

import (
	"fmt"
	"github.com/dotcloud/docker"
	"github.com/dotcloud/docker/archive"
	"github.com/dotcloud/docker/engine"
	"github.com/dotcloud/docker/utils"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mkTestContext generates a build context from the contents of the provided dockerfile.
// This context is suitable for use as an argument to BuildFile.Build()
func mkTestContext(dockerfile string, files [][2]string, t *testing.T) archive.Archive {
	context, err := docker.MkBuildContext(dockerfile, files)
	if err != nil {
		t.Fatal(err)
	}
	return context
}

// A testContextTemplate describes a build context and how to test it
type testContextTemplate struct {
	// Contents of the Dockerfile
	dockerfile string
	// Additional files in the context, eg [][2]string{"./passwd", "gordon"}
	files [][2]string
	// Additional remote files to host on a local HTTP server.
	remoteFiles [][2]string
}

// A table of all the contexts to build and test.
// A new docker runtime will be created and torn down for each context.
var testContexts = []testContextTemplate{
	{
		`
from   {IMAGE}
run    sh -c 'echo root:testpass > /tmp/passwd'
run    mkdir -p /var/run/sshd
run    [ "$(cat /tmp/passwd)" = "root:testpass" ]
run    [ "$(ls -d /var/run/sshd)" = "/var/run/sshd" ]
`,
		nil,
		nil,
	},

	// Exactly the same as above, except uses a line split with a \ to test
	// multiline support.
	{
		`
from   {IMAGE}
run    sh -c 'echo root:testpass \
	> /tmp/passwd'
run    mkdir -p /var/run/sshd
run    [ "$(cat /tmp/passwd)" = "root:testpass" ]
run    [ "$(ls -d /var/run/sshd)" = "/var/run/sshd" ]
`,
		nil,
		nil,
	},

	// Line containing literal "\n"
	{
		`
from   {IMAGE}
run    sh -c 'echo root:testpass > /tmp/passwd'
run    echo "foo \n bar"; echo "baz"
run    mkdir -p /var/run/sshd
run    [ "$(cat /tmp/passwd)" = "root:testpass" ]
run    [ "$(ls -d /var/run/sshd)" = "/var/run/sshd" ]
`,
		nil,
		nil,
	},
	{
		`
from {IMAGE}
add foo /usr/lib/bla/bar
run [ "$(cat /usr/lib/bla/bar)" = 'hello' ]
add http://{SERVERADDR}/baz /usr/lib/baz/quux
run [ "$(cat /usr/lib/baz/quux)" = 'world!' ]
`,
		[][2]string{{"foo", "hello"}},
		[][2]string{{"/baz", "world!"}},
	},

	{
		`
from {IMAGE}
add f /
run [ "$(cat /f)" = "hello" ]
add f /abc
run [ "$(cat /abc)" = "hello" ]
add f /x/y/z
run [ "$(cat /x/y/z)" = "hello" ]
add f /x/y/d/
run [ "$(cat /x/y/d/f)" = "hello" ]
add d /
run [ "$(cat /ga)" = "bu" ]
add d /somewhere
run [ "$(cat /somewhere/ga)" = "bu" ]
add d /anotherplace/
run [ "$(cat /anotherplace/ga)" = "bu" ]
add d /somewheeeere/over/the/rainbooow
run [ "$(cat /somewheeeere/over/the/rainbooow/ga)" = "bu" ]
`,
		[][2]string{
			{"f", "hello"},
			{"d/ga", "bu"},
		},
		nil,
	},

	{
		`
from {IMAGE}
add http://{SERVERADDR}/x /a/b/c
run [ "$(cat /a/b/c)" = "hello" ]
add http://{SERVERADDR}/x?foo=bar /
run [ "$(cat /x)" = "hello" ]
add http://{SERVERADDR}/x /d/
run [ "$(cat /d/x)" = "hello" ]
add http://{SERVERADDR} /e
run [ "$(cat /e)" = "blah" ]
`,
		nil,
		[][2]string{{"/x", "hello"}, {"/", "blah"}},
	},

	// Comments, shebangs, and executability, oh my!
	{
		`
FROM {IMAGE}
# This is an ordinary comment.
RUN { echo '#!/bin/sh'; echo 'echo hello world'; } > /hello.sh
RUN [ ! -x /hello.sh ]
RUN chmod +x /hello.sh
RUN [ -x /hello.sh ]
RUN [ "$(cat /hello.sh)" = $'#!/bin/sh\necho hello world' ]
RUN [ "$(/hello.sh)" = "hello world" ]
`,
		nil,
		nil,
	},

	// Environment variable
	{
		`
from   {IMAGE}
env    FOO BAR
run    [ "$FOO" = "BAR" ]
`,
		nil,
		nil,
	},

	// Environment overwriting
	{
		`
from   {IMAGE}
env    FOO BAR
run    [ "$FOO" = "BAR" ]
env    FOO BAZ
run    [ "$FOO" = "BAZ" ]
`,
		nil,
		nil,
	},

	{
		`
from {IMAGE}
ENTRYPOINT /bin/echo
CMD Hello world
`,
		nil,
		nil,
	},

	{
		`
from {IMAGE}
VOLUME /test
CMD Hello world
`,
		nil,
		nil,
	},

	{
		`
from {IMAGE}
env    FOO /foo/baz
env    BAR /bar
env    BAZ $BAR
env    FOOPATH $PATH:$FOO
run    [ "$BAR" = "$BAZ" ]
run    [ "$FOOPATH" = "$PATH:/foo/baz" ]
`,
		nil,
		nil,
	},

	{
		`
from {IMAGE}
env    FOO /bar
env    TEST testdir
env    BAZ /foobar
add    testfile $BAZ/
add    $TEST $FOO
run    [ "$(cat /foobar/testfile)" = "test1" ]
run    [ "$(cat /bar/withfile)" = "test2" ]
`,
		[][2]string{
			{"testfile", "test1"},
			{"testdir/withfile", "test2"},
		},
		nil,
	},
}

// FIXME: test building with 2 successive overlapping ADD commands

func constructDockerfile(template string, ip net.IP, port string) string {
	serverAddr := fmt.Sprintf("%s:%s", ip, port)
	replacer := strings.NewReplacer("{IMAGE}", unitTestImageID, "{SERVERADDR}", serverAddr)
	return replacer.Replace(template)
}

func mkTestingFileServer(files [][2]string) (*httptest.Server, error) {
	mux := http.NewServeMux()
	for _, file := range files {
		name, contents := file[0], file[1]
		mux.HandleFunc(name, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(contents))
		})
	}

	// This is how httptest.NewServer sets up a net.Listener, except that our listener must accept remote
	// connections (from the container).
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}

	s := httptest.NewUnstartedServer(mux)
	s.Listener = listener
	s.Start()
	return s, nil
}

func TestBuild(t *testing.T) {
	for _, ctx := range testContexts {
		_, err := buildImage(ctx, t, nil, true)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func buildImage(context testContextTemplate, t *testing.T, eng *engine.Engine, useCache bool) (*docker.Image, error) {
	if eng == nil {
		eng = NewTestEngine(t)
		runtime := mkRuntimeFromEngine(eng, t)
		// FIXME: we might not need runtime, why not simply nuke
		// the engine?
		defer nuke(runtime)
	}
	srv := mkServerFromEngine(eng, t)

	httpServer, err := mkTestingFileServer(context.remoteFiles)
	if err != nil {
		t.Fatal(err)
	}
	defer httpServer.Close()

	idx := strings.LastIndex(httpServer.URL, ":")
	if idx < 0 {
		t.Fatalf("could not get port from test http server address %s", httpServer.URL)
	}
	port := httpServer.URL[idx+1:]

	iIP := eng.Hack_GetGlobalVar("httpapi.bridgeIP")
	if iIP == nil {
		t.Fatal("Legacy bridgeIP field not set in engine")
	}
	ip, ok := iIP.(net.IP)
	if !ok {
		panic("Legacy bridgeIP field in engine does not cast to net.IP")
	}
	dockerfile := constructDockerfile(context.dockerfile, ip, port)

	buildfile := docker.NewBuildFile(srv, ioutil.Discard, ioutil.Discard, false, useCache, false, ioutil.Discard, utils.NewStreamFormatter(false), nil)
	id, err := buildfile.Build(mkTestContext(dockerfile, context.files, t))
	if err != nil {
		return nil, err
	}

	return srv.ImageInspect(id)
}

func TestVolume(t *testing.T) {
	img, err := buildImage(testContextTemplate{`
        from {IMAGE}
        volume /test
        cmd Hello world
    `, nil, nil}, t, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	if len(img.Config.Volumes) == 0 {
		t.Fail()
	}
	for key := range img.Config.Volumes {
		if key != "/test" {
			t.Fail()
		}
	}
}

func TestBuildMaintainer(t *testing.T) {
	img, err := buildImage(testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
    `, nil, nil}, t, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	if img.Author != "dockerio" {
		t.Fail()
	}
}

func TestBuildUser(t *testing.T) {
	img, err := buildImage(testContextTemplate{`
        from {IMAGE}
        user dockerio
    `, nil, nil}, t, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	if img.Config.User != "dockerio" {
		t.Fail()
	}
}

func TestBuildEnv(t *testing.T) {
	img, err := buildImage(testContextTemplate{`
        from {IMAGE}
        env port 4243
        `,
		nil, nil}, t, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	hasEnv := false
	for _, envVar := range img.Config.Env {
		if envVar == "port=4243" {
			hasEnv = true
			break
		}
	}
	if !hasEnv {
		t.Fail()
	}
}

func TestBuildCmd(t *testing.T) {
	img, err := buildImage(testContextTemplate{`
        from {IMAGE}
        cmd ["/bin/echo", "Hello World"]
        `,
		nil, nil}, t, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	if img.Config.Cmd[0] != "/bin/echo" {
		t.Log(img.Config.Cmd[0])
		t.Fail()
	}
	if img.Config.Cmd[1] != "Hello World" {
		t.Log(img.Config.Cmd[1])
		t.Fail()
	}
}

func TestBuildExpose(t *testing.T) {
	img, err := buildImage(testContextTemplate{`
        from {IMAGE}
        expose 4243
        `,
		nil, nil}, t, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	if img.Config.PortSpecs[0] != "4243" {
		t.Fail()
	}
}

func TestBuildEntrypoint(t *testing.T) {
	img, err := buildImage(testContextTemplate{`
        from {IMAGE}
        entrypoint ["/bin/echo"]
        `,
		nil, nil}, t, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	if img.Config.Entrypoint[0] != "/bin/echo" {
		t.Log(img.Config.Entrypoint[0])
		t.Fail()
	}
}

// testing #1405 - config.Cmd does not get cleaned up if
// utilizing cache
func TestBuildEntrypointRunCleanup(t *testing.T) {
	eng := NewTestEngine(t)
	defer nuke(mkRuntimeFromEngine(eng, t))

	img, err := buildImage(testContextTemplate{`
        from {IMAGE}
        run echo "hello"
        `,
		nil, nil}, t, eng, true)
	if err != nil {
		t.Fatal(err)
	}

	img, err = buildImage(testContextTemplate{`
        from {IMAGE}
        run echo "hello"
        add foo /foo
        entrypoint ["/bin/echo"]
        `,
		[][2]string{{"foo", "HEYO"}}, nil}, t, eng, true)
	if err != nil {
		t.Fatal(err)
	}

	if len(img.Config.Cmd) != 0 {
		t.Fail()
	}
}

func checkCacheBehavior(t *testing.T, template testContextTemplate, expectHit bool) (imageId string) {
	eng := NewTestEngine(t)
	defer nuke(mkRuntimeFromEngine(eng, t))

	img, err := buildImage(template, t, eng, true)
	if err != nil {
		t.Fatal(err)
	}

	imageId = img.ID

	img, err = buildImage(template, t, eng, expectHit)
	if err != nil {
		t.Fatal(err)
	}

	if hit := imageId == img.ID; hit != expectHit {
		t.Fatalf("Cache misbehavior, got hit=%t, expected hit=%t: (first: %s, second %s)", hit, expectHit, imageId, img.ID)
	}
	return
}

func checkCacheBehaviorFromEngime(t *testing.T, template testContextTemplate, expectHit bool, eng *engine.Engine) (imageId string) {
	img, err := buildImage(template, t, eng, true)
	if err != nil {
		t.Fatal(err)
	}

	imageId = img.ID

	img, err = buildImage(template, t, eng, expectHit)
	if err != nil {
		t.Fatal(err)
	}

	if hit := imageId == img.ID; hit != expectHit {
		t.Fatalf("Cache misbehavior, got hit=%t, expected hit=%t: (first: %s, second %s)", hit, expectHit, imageId, img.ID)
	}
	return
}

func TestBuildImageWithCache(t *testing.T) {
	template := testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
        `,
		nil, nil}
	checkCacheBehavior(t, template, true)
}

func TestBuildImageWithoutCache(t *testing.T) {
	template := testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
        `,
		nil, nil}
	checkCacheBehavior(t, template, false)
}

func TestBuildADDLocalFileWithCache(t *testing.T) {
	template := testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
        run echo "first"
        add foo /usr/lib/bla/bar
	run [ "$(cat /usr/lib/bla/bar)" = "hello" ]
        run echo "second"
	add . /src/
	run [ "$(cat /src/foo)" = "hello" ]
        `,
		[][2]string{
			{"foo", "hello"},
		},
		nil}
	eng := NewTestEngine(t)
	defer nuke(mkRuntimeFromEngine(eng, t))

	id1 := checkCacheBehaviorFromEngime(t, template, true, eng)
	template.files = append(template.files, [2]string{"bar", "hello2"})
	id2 := checkCacheBehaviorFromEngime(t, template, true, eng)
	if id1 == id2 {
		t.Fatal("The cache should have been invalided but hasn't.")
	}
	id3 := checkCacheBehaviorFromEngime(t, template, true, eng)
	if id2 != id3 {
		t.Fatal("The cache should have been used but hasn't.")
	}
	template.files[1][1] = "hello3"
	id4 := checkCacheBehaviorFromEngime(t, template, true, eng)
	if id3 == id4 {
		t.Fatal("The cache should have been invalided but hasn't.")
	}
	template.dockerfile += `
	add ./bar /src2/
	run ls /src2/bar
	`
	id5 := checkCacheBehaviorFromEngime(t, template, true, eng)
	if id4 == id5 {
		t.Fatal("The cache should have been invalided but hasn't.")
	}
	template.files[1][1] = "hello4"
	id6 := checkCacheBehaviorFromEngime(t, template, true, eng)
	if id5 == id6 {
		t.Fatal("The cache should have been invalided but hasn't.")
	}

	template.dockerfile += `
	add bar /src2/bar2
	add /bar /src2/bar3
	run ls /src2/bar2 /src2/bar3
	`
	id7 := checkCacheBehaviorFromEngime(t, template, true, eng)
	if id6 == id7 {
		t.Fatal("The cache should have been invalided but hasn't.")
	}
	template.files[1][1] = "hello5"
	id8 := checkCacheBehaviorFromEngime(t, template, true, eng)
	if id7 == id8 {
		t.Fatal("The cache should have been invalided but hasn't.")
	}
}

func TestBuildADDLocalFileWithoutCache(t *testing.T) {
	template := testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
        run echo "first"
        add foo /usr/lib/bla/bar
        run echo "second"
        `,
		[][2]string{{"foo", "hello"}},
		nil}
	checkCacheBehavior(t, template, false)
}

func TestBuildADDCurrentDirectoryWithCache(t *testing.T) {
	template := testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
        add . /usr/lib/bla
        `,
		nil, nil}
	checkCacheBehavior(t, template, true)
}

func TestBuildADDCurrentDirectoryWithoutCache(t *testing.T) {
	template := testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
        add . /usr/lib/bla
        `,
		nil, nil}
	checkCacheBehavior(t, template, false)
}

func TestBuildADDRemoteFileWithCache(t *testing.T) {
	template := testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
        run echo "first"
        add http://{SERVERADDR}/baz /usr/lib/baz/quux
        run echo "second"
        `,
		nil,
		[][2]string{{"/baz", "world!"}}}
	checkCacheBehavior(t, template, true)
}

func TestBuildADDRemoteFileWithoutCache(t *testing.T) {
	template := testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
        run echo "first"
        add http://{SERVERADDR}/baz /usr/lib/baz/quux
        run echo "second"
        `,
		nil,
		[][2]string{{"/baz", "world!"}}}
	checkCacheBehavior(t, template, false)
}

func TestBuildADDLocalAndRemoteFilesWithCache(t *testing.T) {
	template := testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
        run echo "first"
        add foo /usr/lib/bla/bar
        add http://{SERVERADDR}/baz /usr/lib/baz/quux
        run echo "second"
        `,
		[][2]string{{"foo", "hello"}},
		[][2]string{{"/baz", "world!"}}}
	checkCacheBehavior(t, template, true)
}

func TestBuildADDLocalAndRemoteFilesWithoutCache(t *testing.T) {
	template := testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
        run echo "first"
        add foo /usr/lib/bla/bar
        add http://{SERVERADDR}/baz /usr/lib/baz/quux
        run echo "second"
        `,
		[][2]string{{"foo", "hello"}},
		[][2]string{{"/baz", "world!"}}}
	checkCacheBehavior(t, template, false)
}

func TestForbiddenContextPath(t *testing.T) {
	eng := NewTestEngine(t)
	defer nuke(mkRuntimeFromEngine(eng, t))
	srv := mkServerFromEngine(eng, t)

	context := testContextTemplate{`
        from {IMAGE}
        maintainer dockerio
        add ../../ test/
        `,
		[][2]string{{"test.txt", "test1"}, {"other.txt", "other"}}, nil}

	httpServer, err := mkTestingFileServer(context.remoteFiles)
	if err != nil {
		t.Fatal(err)
	}
	defer httpServer.Close()

	idx := strings.LastIndex(httpServer.URL, ":")
	if idx < 0 {
		t.Fatalf("could not get port from test http server address %s", httpServer.URL)
	}
	port := httpServer.URL[idx+1:]

	iIP := eng.Hack_GetGlobalVar("httpapi.bridgeIP")
	if iIP == nil {
		t.Fatal("Legacy bridgeIP field not set in engine")
	}
	ip, ok := iIP.(net.IP)
	if !ok {
		panic("Legacy bridgeIP field in engine does not cast to net.IP")
	}
	dockerfile := constructDockerfile(context.dockerfile, ip, port)

	buildfile := docker.NewBuildFile(srv, ioutil.Discard, ioutil.Discard, false, true, false, ioutil.Discard, utils.NewStreamFormatter(false), nil)
	_, err = buildfile.Build(mkTestContext(dockerfile, context.files, t))

	if err == nil {
		t.Log("Error should not be nil")
		t.Fail()
	}

	if err.Error() != "Forbidden path outside the build context: ../../ (/)" {
		t.Logf("Error message is not expected: %s", err.Error())
		t.Fail()
	}
}

func TestBuildADDFileNotFound(t *testing.T) {
	eng := NewTestEngine(t)
	defer nuke(mkRuntimeFromEngine(eng, t))

	context := testContextTemplate{`
        from {IMAGE}
        add foo /usr/local/bar
        `,
		nil, nil}

	httpServer, err := mkTestingFileServer(context.remoteFiles)
	if err != nil {
		t.Fatal(err)
	}
	defer httpServer.Close()

	idx := strings.LastIndex(httpServer.URL, ":")
	if idx < 0 {
		t.Fatalf("could not get port from test http server address %s", httpServer.URL)
	}
	port := httpServer.URL[idx+1:]

	iIP := eng.Hack_GetGlobalVar("httpapi.bridgeIP")
	if iIP == nil {
		t.Fatal("Legacy bridgeIP field not set in engine")
	}
	ip, ok := iIP.(net.IP)
	if !ok {
		panic("Legacy bridgeIP field in engine does not cast to net.IP")
	}
	dockerfile := constructDockerfile(context.dockerfile, ip, port)

	buildfile := docker.NewBuildFile(mkServerFromEngine(eng, t), ioutil.Discard, ioutil.Discard, false, true, false, ioutil.Discard, utils.NewStreamFormatter(false), nil)
	_, err = buildfile.Build(mkTestContext(dockerfile, context.files, t))

	if err == nil {
		t.Log("Error should not be nil")
		t.Fail()
	}

	if err.Error() != "foo: no such file or directory" {
		t.Logf("Error message is not expected: %s", err.Error())
		t.Fail()
	}
}

func TestBuildInheritance(t *testing.T) {
	eng := NewTestEngine(t)
	defer nuke(mkRuntimeFromEngine(eng, t))

	img, err := buildImage(testContextTemplate{`
            from {IMAGE}
            expose 4243
            `,
		nil, nil}, t, eng, true)

	if err != nil {
		t.Fatal(err)
	}

	img2, _ := buildImage(testContextTemplate{fmt.Sprintf(`
            from %s
            entrypoint ["/bin/echo"]
            `, img.ID),
		nil, nil}, t, eng, true)

	if err != nil {
		t.Fatal(err)
	}

	// from child
	if img2.Config.Entrypoint[0] != "/bin/echo" {
		t.Fail()
	}

	// from parent
	if img.Config.PortSpecs[0] != "4243" {
		t.Fail()
	}
}

func TestBuildFails(t *testing.T) {
	_, err := buildImage(testContextTemplate{`
        from {IMAGE}
        run sh -c "exit 23"
        `,
		nil, nil}, t, nil, true)

	if err == nil {
		t.Fatal("Error should not be nil")
	}

	sterr, ok := err.(*utils.JSONError)
	if !ok {
		t.Fatalf("Error should be utils.JSONError")
	}
	if sterr.Code != 23 {
		t.Fatalf("StatusCode %d unexpected, should be 23", sterr.Code)
	}
}

func TestBuildFailsDockerfileEmpty(t *testing.T) {
	_, err := buildImage(testContextTemplate{``, nil, nil}, t, nil, true)

	if err != docker.ErrDockerfileEmpty {
		t.Fatal("Expected: %v, got: %v", docker.ErrDockerfileEmpty, err)
	}
}
