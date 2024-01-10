package main

import (
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"
	"go.elara.ws/logger/log"
	"go.elara.ws/pcre"
	"go.elara.ws/salix"
)

type Config struct {
	File         *ConfigFile
	PollInterval time.Duration
	Regexes      map[string]*pcre.Regexp
	Tmpls        *salix.Namespace
}

type ConfigFile struct {
	Lemmy   Lemmy   `toml:"lemmy"`
	Replies []Reply `toml:"reply"`
}

type Lemmy struct {
	InstanceURL  string       `toml:"instance_url"`
	PollInterval string       `toml:"poll_interval"`
	Account      LemmyAccount `toml:"account"`
}

type LemmyAccount struct {
	UserOrEmail string `toml:"user_or_email"`
	Password    string `toml:"password"`
}

type Reply struct {
	Regex    string `toml:"regex"`
	Template string `toml:"template"`
}

func loadConfig(path string) (Config, error) {
	fl, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}

	fi, err := fl.Stat()
	if err != nil {
		return Config{}, err
	}

	if fi.Mode().Perm() != 0o600 {
		log.Fatal("Your config file's permissions are insecure. Please use chmod to set them to 600. Refusing to start.").Send()
	}

	cfgFile := &ConfigFile{Lemmy: Lemmy{PollInterval: "10s"}}
	err = toml.NewDecoder(fl).Decode(cfgFile)
	if err != nil {
		return Config{}, err
	}

	out := Config{File: cfgFile}

	out.Regexes, out.Tmpls, err = compileReplies(cfgFile.Replies)
	if err != nil {
		return Config{}, err
	}

	out.PollInterval, err = time.ParseDuration(cfgFile.Lemmy.PollInterval)
	if err != nil {
		return Config{}, err
	}

	return out, nil
}

func compileReplies(replies []Reply) (map[string]*pcre.Regexp, *salix.Namespace, error) {
	regexes := map[string]*pcre.Regexp{}
	ns := salix.New().WithVarMap(map[string]any{
		"regexReplace": regexReplace,
	})

	for _, reply := range replies {
		if _, ok := regexes[reply.Regex]; ok {
			continue
		}

		re, err := pcre.Compile(reply.Regex)
		if err != nil {
			return nil, nil, err
		}
		regexes[reply.Regex] = re

		_, err = ns.ParseString(reply.Regex, reply.Template)
		if err != nil {
			return nil, nil, err
		}
	}

	return regexes, ns, nil
}

func regexReplace(str, pattern, new string) (string, error) {
	re, err := pcre.Compile(pattern)
	if err != nil {
		return "", err
	}
	return re.ReplaceAllString(str, new), nil
}
