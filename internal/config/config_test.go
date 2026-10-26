package config

import (
	"flag"
	"math"
	"os"
	"reflect"
	"strconv"
	"testing"
)

func resetFlagsForTest(t *testing.T, args []string) func() {
	t.Helper()

	prevCommandLine := flag.CommandLine
	prevArgs := os.Args
	prevDockerLabelPrefix := allowedDockerLabelPrefix

	flag.CommandLine = flag.NewFlagSet(args[0], flag.ContinueOnError)
	flag.CommandLine.SetOutput(os.Stderr)
	os.Args = args

	return func() {
		flag.CommandLine = prevCommandLine
		os.Args = prevArgs
		allowedDockerLabelPrefix = prevDockerLabelPrefix
	}
}

func TestInitConfig_AllowMethodFlagOverridesEnv(t *testing.T) {
	t.Setenv("SP_ALLOW_GET", "/from-env")
	restore := resetFlagsForTest(t, []string{"socket-proxy", "-allowGET=/from-flag"})
	defer restore()

	cfg, err := InitConfig()
	if err != nil {
		t.Fatalf("InitConfig() error = %v", err)
	}

	regexes := cfg.AllowLists.Default.AllowedRequests["GET"]
	if len(regexes) != 1 {
		t.Fatalf("expected 1 GET regex, got %d", len(regexes))
	}
	if !regexes[0].MatchString("/from-flag") {
		t.Fatalf("expected GET regex to match /from-flag, got %q", regexes[0].String())
	}
	if regexes[0].MatchString("/from-env") {
		t.Fatalf("expected env GET regex to be ignored when flag is present, got %q", regexes[0].String())
	}
}

func TestInitConfig_AllowedContainerNames(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		args    []string
		want    []string
		wantErr bool
	}{
		{name: "default"},
		{name: "environment", value: "db, redis", want: []string{"db", "redis"}},
		{name: "flag overrides environment", value: "db", args: []string{"-allowcontainername=redis"}, want: []string{"redis"}},
		{name: "empty flag disables filter", value: "db", args: []string{"-allowcontainername="}},
		{name: "empty prefix", value: "db,", wantErr: true},
		{name: "invalid prefix", value: "db,*", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("SP_ALLOWCONTAINERNAME", test.value)
			restore := resetFlagsForTest(t, append([]string{"socket-proxy"}, test.args...))
			defer restore()
			cfg, err := InitConfig()
			if (err != nil) != test.wantErr {
				t.Fatalf("InitConfig() error = %v", err)
			}
			if err == nil && !reflect.DeepEqual(cfg.AllowLists.Default.AllowedContainerNames, test.want) {
				t.Errorf("prefixes = %v, want %v", cfg.AllowLists.Default.AllowedContainerNames, test.want)
			}
		})
	}
}

func TestInitConfig_DockerLabelPrefixFlagOverridesEnv(t *testing.T) {
	t.Setenv("SP_DOCKERLABELPREFIX", "from-env")
	restore := resetFlagsForTest(t, []string{"socket-proxy", "-dockerlabelprefix=gameserver-socket-proxy"})
	defer restore()

	_, err := InitConfig()
	if err != nil {
		t.Fatalf("InitConfig() error = %v", err)
	}

	if got, want := allowedDockerLabelPrefix, "gameserver-socket-proxy.allow."; got != want {
		t.Errorf("allowedDockerLabelPrefix = %q, want %q", got, want)
	}
}

func TestInitConfig_AllowedContainerLabels(t *testing.T) {
	for _, test := range []struct {
		name     string
		value    string
		prefixed *string
		args     []string
		want     []string
		wantErr  bool
	}{
		{name: "default"},
		{name: "environment", value: "myfoo, com.bar=fun", want: []string{"myfoo", "com.bar=fun"}},
		{name: "prefixed environment wins", value: "ignored", prefixed: new("myfoo"), want: []string{"myfoo"}},
		{name: "empty prefixed environment disables", value: "ignored", prefixed: new("")},
		{name: "flag overrides environment", value: "ignored", args: []string{"-allowcontainerlabels=com.bar=fun"}, want: []string{"com.bar=fun"}},
		{name: "empty flag disables", value: "ignored", args: []string{"-allowcontainerlabels="}},
		{name: "empty value", value: "myfoo=", want: []string{"myfoo="}},
		{name: "equals in value", value: "myfoo=a=b", want: []string{"myfoo=a=b"}},
		{name: "empty entry", value: "myfoo,", wantErr: true},
		{name: "empty key", value: "=fun", wantErr: true},
		{name: "space in key", value: "my foo", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CONTAINER_LABELS", test.value)
			t.Setenv("SP_ALLOWCONTAINERLABELS", "")
			if err := os.Unsetenv("SP_ALLOWCONTAINERLABELS"); err != nil {
				t.Fatal(err)
			}
			if test.prefixed != nil {
				t.Setenv("SP_ALLOWCONTAINERLABELS", *test.prefixed)
			}
			restore := resetFlagsForTest(t, append([]string{"socket-proxy"}, test.args...))
			defer restore()
			cfg, err := InitConfig()
			if (err != nil) != test.wantErr {
				t.Fatalf("InitConfig() error = %v", err)
			}
			if err == nil && !reflect.DeepEqual(cfg.AllowLists.Default.AllowedContainerLabels, test.want) {
				t.Errorf("labels = %v, want %v", cfg.AllowLists.Default.AllowedContainerLabels, test.want)
			}
		})
	}
}

func TestInitConfig_InvalidDockerLabelPrefix(t *testing.T) {
	for _, prefix := range []string{
		"Traefik",
		"traefik..proxy",
		"traefik-",
	} {
		t.Run(prefix, func(t *testing.T) {
			restore := resetFlagsForTest(t, []string{"socket-proxy", "-dockerlabelprefix=" + prefix})
			defer restore()

			if _, err := InitConfig(); err == nil {
				t.Fatalf("InitConfig() with dockerlabelprefix %q unexpectedly succeeded", prefix)
			}
		})
	}
}

func TestInitConfig_ShutdownGraceTimeTooLarge(t *testing.T) {
	restore := resetFlagsForTest(t, []string{
		"socket-proxy",
		"-shutdowngracetime=" + strconv.FormatUint(uint64(math.MaxInt)+1, 10),
	})
	defer restore()

	_, err := InitConfig()
	if err == nil {
		t.Fatal("InitConfig() unexpectedly succeeded")
	}
}

func TestInitConfig_WatchdogIntervalTooLarge(t *testing.T) {
	restore := resetFlagsForTest(t, []string{
		"socket-proxy",
		"-watchdoginterval=" + strconv.FormatUint(uint64(math.MaxInt)+1, 10),
	})
	defer restore()

	_, err := InitConfig()
	if err == nil {
		t.Fatal("InitConfig() unexpectedly succeeded")
	}
}
