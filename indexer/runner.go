package indexer

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/cardigann/cardigann/config"
	"github.com/cardigann/cardigann/torznab"
	"github.com/dustin/go-humanize"
	"github.com/headzoo/surf"
	"github.com/headzoo/surf/agent"
	"github.com/headzoo/surf/browser"
)

var (
	_ torznab.Indexer = &Runner{}
)

type Runner struct {
	Definition *IndexerDefinition
	Browser    browser.Browsable
	Config     config.Config
	Logger     logrus.FieldLogger
	caps       torznab.Capabilities
}

func NewRunner(def *IndexerDefinition, conf config.Config) *Runner {
	bow := surf.NewBrowser()
	bow.SetUserAgent(agent.Chrome())
	bow.SetAttribute(browser.SendReferer, false)
	bow.SetAttribute(browser.MetaRefreshHandling, false)

	logger := logrus.New()
	logger.Level = logrus.DebugLevel

	return &Runner{
		Definition: def,
		Browser:    bow,
		Config:     conf,
		Logger:     logger.WithFields(logrus.Fields{"site": def.Site}),
	}
}

func (r *Runner) applyTemplate(name, tpl string, ctx interface{}) (string, error) {
	tmpl, err := template.New(name).Parse(tpl)
	if err != nil {
		return "", err
	}
	b := &bytes.Buffer{}
	err = tmpl.Execute(b, ctx)
	if err != nil {
		return "", err
	}
	return b.String(), nil
}

func (r *Runner) currentURL() (*url.URL, error) {
	if u := r.Browser.Url(); u != nil {
		return u, nil
	}

	if configURL, ok, _ := r.Config.Get(r.Definition.Site, "url"); ok {
		return url.Parse(configURL)
	}

	return url.Parse(r.Definition.Links[0])
}

func (r *Runner) resolvePath(urlPath string) (string, error) {
	base, err := r.currentURL()
	if err != nil {
		return "", err
	}

	u, err := url.Parse(urlPath)
	if err != nil {
		log.Fatal(err)
	}

	resolved := base.ResolveReference(u)
	r.Logger.
		WithFields(logrus.Fields{"base": base.String(), "u": resolved.String()}).
		Debugf("Resolving url")

	return resolved.String(), nil
}

func (r *Runner) openPage(u string) error {
	r.Logger.WithField("url", u).Debug("Attempting to open page")

	err := r.Browser.Open(u)
	if err != nil {
		return err
	}

	r.Logger.
		WithFields(logrus.Fields{"code": r.Browser.StatusCode(), "page": r.Browser.Url()}).
		Debugf("Finished request")

	tmpfile, err := ioutil.TempFile("", r.Definition.Site)
	if err != nil {
		return err
	}

	body := strings.NewReader(r.Browser.Body())
	io.Copy(tmpfile, body)
	defer tmpfile.Close()

	r.Logger.
		WithFields(logrus.Fields{"file": "file://" + tmpfile.Name()}).
		Debugf("Wrote page output to cache")

	return nil
}

func (r *Runner) Login() error {
	filterLogger = r.Logger
	filterCategoryMapping = r.Capabilities().Categories

	loginUrl, err := r.resolvePath(r.Definition.Login.Path)
	if err != nil {
		return err
	}

	if err = r.openPage(loginUrl); err != nil {
		return err
	}

	fm, err := r.Browser.Form(r.Definition.Login.FormSelector)
	if err != nil {
		return err
	}

	for name, val := range r.Definition.Login.Inputs {
		r.Logger.
			WithFields(logrus.Fields{"key": name, "form": r.Definition.Login.FormSelector, "val": val}).
			Debugf("Filling input of form")

		cfg, err := r.Config.Section(r.Definition.Site)
		if err != nil {
			return err
		}

		resolved, err := r.applyTemplate("login_inputs", val, struct {
			Config map[string]string
		}{
			cfg,
		})
		if err != nil {
			return err
		}

		r.Logger.
			WithFields(logrus.Fields{"key": name, "form": r.Definition.Login.FormSelector, "val": resolved}).
			Debugf("Resolved input template")

		if err = fm.Input(name, resolved); err != nil {
			return err
		}
	}

	r.Logger.Debug("Submitting login form")

	if err = fm.Submit(); err != nil {
		r.Logger.WithError(err).Error("Login failed")
		return err
	}

	r.Logger.
		WithFields(logrus.Fields{"code": r.Browser.StatusCode(), "page": r.Browser.Url()}).
		Debugf("Finished request")

	if err = r.Definition.Login.hasError(r.Browser); err != nil {
		r.Logger.WithError(err).Error("Failed to login")
		return err
	}

	r.Logger.Info("Successfully logged in")
	return nil
}

func (r *Runner) Info() torznab.Info {
	return torznab.Info{
		ID:       r.Definition.Site,
		Title:    r.Definition.Name,
		Language: r.Definition.Language,
	}
}

func (r *Runner) Test() error {
	filterLogger = r.Logger

	for _, mode := range r.Capabilities().SearchModes {
		query := torznab.Query{
			"t":     mode.Key,
			"limit": 5,
		}

		switch mode.Key {
		case "tv-search":
			query["cat"] = []int{
				torznab.CategoryTV.ID,
				torznab.CategoryTV_HD.ID,
				torznab.CategoryTV_SD.ID,
			}
		}

		r.Logger.Infof("Testing search mode %q", mode.Key)
		results, err := r.Search(query)
		if err != nil {
			return err
		}
		if len(results) == 0 {
			return torznab.ErrNoSuchItem
		}
		for idx, result := range results {
			if result.Title == "" {
				return fmt.Errorf("Result row %d has empty title", idx+1)
			}
			if result.Size == 0 {
				return fmt.Errorf("Result row %d has zero size", idx+1)
			}
			if result.Link == "" {
				return fmt.Errorf("Result row %d has blank link", idx+1)
			}
			if result.Site == "" {
				return fmt.Errorf("Result row %d has blank site", idx+1)
			}
			if result.Category == 0 {
				return fmt.Errorf("Result row %d has blank category", idx+1)
			}
		}
	}

	return nil
}

func (r *Runner) Capabilities() torznab.Capabilities {
	return torznab.Capabilities(r.Definition.Capabilities)
}

func (r *Runner) Search(query torznab.Query) ([]torznab.ResultItem, error) {
	filterLogger = r.Logger
	filterCategoryMapping = r.Capabilities().Categories

	searchUrl, err := r.resolvePath(r.Definition.Search.Path)
	if err != nil {
		return nil, err
	}

	r.Logger.
		WithFields(logrus.Fields{"query": query}).
		Infof("Searching indexer")

	if err := r.openPage(searchUrl); err != nil {
		return nil, err
	}

	localCats := []int{}

	if unmappedCats, ok := query["cat"].([]int); ok {
		localCats = r.Capabilities().Categories.ReverseMap(unmappedCats)
	}

	inputCtx := struct {
		Query      torznab.Query
		Categories []int
	}{
		query,
		localCats,
	}

	vals := url.Values{}

	for name, val := range r.Definition.Search.Inputs {
		resolved, err := r.applyTemplate("search_inputs", val, inputCtx)
		if err != nil {
			return nil, err
		}
		switch name {
		case "$raw":
			parsedVals, err := url.ParseQuery(resolved)
			if err != nil {
				return nil, fmt.Errorf("Error parsing $raw input: %s", err.Error())
			}

			r.Logger.
				WithFields(logrus.Fields{"source": val, "parsed": parsedVals}).
				Infof("Processed $raw input")

			for k, values := range parsedVals {
				for _, val := range values {
					vals.Add(k, val)
				}
			}
		default:
			vals.Add(name, resolved)
		}
	}

	r.Logger.
		WithFields(logrus.Fields{"params": vals, "page": searchUrl}).
		Debugf("Submitting page with form params")

	err = r.Browser.OpenForm(searchUrl, vals)
	if err != nil {
		return nil, err
	}

	r.Logger.
		WithFields(logrus.Fields{"code": r.Browser.StatusCode(), "page": r.Browser.Url()}).
		Debugf("Finished opening form")

	items := []torznab.ResultItem{}
	timer := time.Now()
	rows := r.Browser.Find(r.Definition.Search.Rows.Selector)
	limit, hasLimit := query["limit"].(int)

	r.Logger.
		WithFields(logrus.Fields{"rows": rows.Length(), "selector": r.Definition.Search.Rows.Selector}).
		Debugf("Found %d rows", rows.Length())

	for i := 0; i < rows.Length() && (!hasLimit || len(items) < limit); i++ {
		row := map[string]string{}

		for field, block := range r.Definition.Search.Fields {
			r.Logger.
				WithFields(logrus.Fields{"row": i + 1, "block": block}).
				Debugf("Processing field %q", field)

			val, err := block.Text(rows.Eq(i))
			if err != nil {
				return nil, err
			}

			r.Logger.
				WithFields(logrus.Fields{"row": i + 1, "output": val}).
				Debugf("Finished processing field %q", field)

			row[field] = val
		}

		item := torznab.ResultItem{
			Site:            r.Definition.Site,
			MinimumRatio:    1,
			MinimumSeedTime: time.Hour * 48,
		}

		r.Logger.
			WithFields(logrus.Fields{"row": i + 1, "data": row}).
			Debugf("Finished row %d", i+1)

		for key, val := range row {
			switch key {
			case "download":
				u, err := r.resolvePath(val)
				if err != nil {
					r.Logger.Warnf("Search result row #%d has malformed url in %s", i+1, key)
					continue
				}
				item.Link = u
			case "details":
				u, err := r.resolvePath(val)
				if err != nil {
					r.Logger.Warnf("Search result row #%d has malformed url in %s", i+1, key)
					continue
				}
				item.GUID = u
			case "comments":
				u, err := r.resolvePath(val)
				if err != nil {
					r.Logger.Warnf("Search result row #%d has malformed url in %s", i+1, key)
					continue
				}
				item.Comments = u
			case "title":
				item.Title = val
			case "description":
				item.Description = val
			case "category":
				catID, err := strconv.Atoi(val)
				if err != nil {
					r.Logger.Warnf("Search result row #%d has malformed categoryid: %s", i+1, err.Error())
					continue
				}
				item.Category = catID
			case "size":
				bytes, err := humanize.ParseBytes(val)
				if err != nil {
					r.Logger.Warnf("Search result row #%d has malformed size: %s", i+1, err.Error())
					continue
				}
				item.Size = bytes
			case "leechers":
				leechers, err := strconv.Atoi(val)
				if err != nil {
					r.Logger.Warnf("Search result row #%d has malformed leechers value in %s", i+1, key)
					continue
				}
				item.Peers += leechers
			case "seeders":
				seeders, err := strconv.Atoi(val)
				if err != nil {
					r.Logger.Warnf("Search result row #%d has malformed seeders value in %s", i+1, key)
					continue
				}
				item.Seeders = seeders
				item.Peers += seeders
			case "date":
				t, err := time.Parse(time.RFC1123Z, val)
				if err != nil {
					r.Logger.Warnf("Search result row #%d has malformed time value in %s", i+1, key)
					continue
				}
				item.PublishDate = t
			default:
				return nil, fmt.Errorf("Unknown field %q", key)
			}
		}

		skipItem := false

		// some trackers have empty rows when there are no results
		if item.Title == "" {
			return nil, nil
		}

		// some trackers don't support filtering by categories, so do it for them
		if catFilters, hasCats := query["cat"].([]int); hasCats {
			var catMatch bool
			for _, catId := range catFilters {
				r.Logger.Debugf("Checking item cat %d against query cat %d", item.Category, catId)
				if catId == item.Category {
					catMatch = true
				}
			}
			if !catMatch {
				r.Logger.Debugf("Skipping row due to non-matching category")
				skipItem = skipItem || !catMatch
			}
		}

		if !skipItem {
			items = append(items, item)
		}
	}

	r.Logger.WithFields(logrus.Fields{"time": time.Now().Sub(timer)}).Infof("Query returned %d results", len(items))
	return items, nil
}

func (r *Runner) Download(u string) (io.ReadCloser, http.Header, error) {
	if err := r.Login(); err != nil {
		return nil, http.Header{}, err
	}

	fullUrl, err := r.resolvePath(u)
	if err != nil {
		return nil, http.Header{}, err
	}

	if err := r.Browser.Open(fullUrl); err != nil {
		return nil, http.Header{}, err
	}

	b := &bytes.Buffer{}

	if _, err := r.Browser.Download(b); err != nil {
		return nil, http.Header{}, err
	}

	return ioutil.NopCloser(bytes.NewReader(b.Bytes())), r.Browser.ResponseHeaders(), nil
}
