package main

import (
	"net/url"
	"os"
	"strconv"
	"text/template"
	"time"

	"github.com/Masterminds/sprig"
	"github.com/pelletier/go-toml/v2"
	"go.elara.ws/logger/log"
	"go.elara.ws/pcre"
)

type ConfigFile struct {
	Lemmy struct {
		InstanceURL string `toml:"instanceURL"`
		Account     struct {
			UserOrEmail string `toml:"userOrEmail"`
			Password    string `toml:"password"`
		} `toml:"account"`
	} `toml:"lemmy"`
	Replies []Reply `toml:"reply"`
}

type Reply struct {
	Regex string `toml:"regex"`
	Msg   string `toml:"msg"`
}

type Config struct {
	ConfigFile   *ConfigFile
	Regexes      map[string]*pcre.Regexp
	Tmpls        map[string]*template.Template
	PollInterval time.Duration
}

func loadConfig(path string) (Config, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Config{}, err
	}

	if fi.Mode().Perm() != 0o600 {
		log.Fatal("Your config file's permissions are insecure. Please use chmod to set them to 600. Refusing to start.").Send()
	}

	fl, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}

	cfgFile := &ConfigFile{}
	err = toml.NewDecoder(fl).Decode(cfgFile)
	if err != nil {
		return Config{}, err
	}

	compiledRegexes, compiledTmpls, err := compileReplies(cfgFile.Replies)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{cfgFile, compiledRegexes, compiledTmpls, 15 * time.Second}
	validateConfig(cfg)
	return cfg, nil
}

func compileReplies(replies []Reply) (map[string]*pcre.Regexp, map[string]*template.Template, error) {
	compiledRegexes := map[string]*pcre.Regexp{}
	compiledTmpls := map[string]*template.Template{}

	for i, reply := range replies {
		if _, ok := compiledRegexes[reply.Regex]; ok {
			continue
		}

		re, err := pcre.Compile(reply.Regex)
		if err != nil {
			return nil, nil, err
		}
		compiledRegexes[reply.Regex] = re

		tmpl, err := template.
			New(strconv.Itoa(i)).
			Funcs(sprig.TxtFuncMap()).
			Parse(reply.Msg)
		if err != nil {
			return nil, nil, err
		}
		compiledTmpls[reply.Regex] = tmpl
	}

	return compiledRegexes, compiledTmpls, nil
}

func validateConfig(cfg Config) {
	_, err := url.Parse(cfg.ConfigFile.Lemmy.InstanceURL)
	if err != nil {
		log.Fatal("Lemmy instance URL is not valid").Err(err).Send()
	}

	for i, reply := range cfg.ConfigFile.Replies {
		re := cfg.Regexes[reply.Regex]

		if re.MatchString(reply.Msg) {
			log.Fatal("Regular expression matches message. This may create an infinite loop. Refusing to start.").Int("reply-index", i).Send()
		}
	}
}
