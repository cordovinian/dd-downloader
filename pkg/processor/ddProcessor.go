package processor

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/DataDog/datadog-api-client-go/v2/api/datadogV2"
	csv_processor "github.com/girishg4t/dd-downloader/pkg/csv"
	dd "github.com/girishg4t/dd-downloader/pkg/datadog"
	"github.com/girishg4t/dd-downloader/pkg/model"
	"github.com/girishg4t/dd-downloader/pkg/util"
)

var ddLogs func(after *string)

var nddLogs func(after *string, ddf model.DataDogFilter)

type YamlProcessor struct {
	Yaml    *model.YamlMapping
	CsvFile string
}

func NewYamlProcessor(y *model.YamlMapping, filename string) YamlProcessor {
	os.Setenv("DD_SITE", y.Spec.Auth.DdSite)
	os.Setenv("DD_API_KEY", y.Spec.Auth.DdAPIKey)
	os.Setenv("DD_APP_KEY", y.Spec.Auth.DdAppKey)
	return YamlProcessor{
		Yaml:    y,
		CsvFile: filename,
	}
}

// validate if the given yaml configuration is mapped to correct logs in datadog
// it will download just 10 records for validation
func (y YamlProcessor) Validate(filename string) (out [][]string, err error) {
	log.Printf("Validate the given yaml %v file with respective datadog logs \n", filename)
	defer func() {
		if r := recover(); r != nil {
			err = r.(error)
		}
	}()
	out = [][]string{}
	var headers []string
	// Custom: add each log attribute
	headers = append(headers, "timestamp")
	headers = append(headers, "service")
	headers = append(headers, "status")
	headers = append(headers, "message")
	headers = append(headers, "attributes")
	util.ReadHeader(y.Yaml.Spec.Mapping, &headers)
	out = append(out, headers)
	logs := dd.GetDataDogLogs(y.Yaml.Spec.DatadogFilter, nil, 10)

	log.Printf("Found records => %d \n", len(logs.Data))
	csvValues, err := y.getLogs(logs.Data)
	if err != nil {
		return nil, err
	}
	out = append(out, csvValues...)
	return out, nil
}

// run the datadog downloader sequentially based on query util all logs are downloaded
func (y YamlProcessor) RunSync() error {
	var headers []string
	// Custom: add each log attribute
	headers = append(headers, "timestamp")
	headers = append(headers, "service")
	headers = append(headers, "status")
	headers = append(headers, "message")
	headers = append(headers, "attributes")
	util.ReadHeader(y.Yaml.Spec.Mapping, &headers)
	csv_processor.CsvWriter(y.CsvFile, headers, nil)
	ddLogs = func(after *string) {
		// Note: Datadog Rate limit seems to be 2 requests per 10 second period
		time.Sleep(6 * time.Second)
		logs := dd.GetDataDogLogs(y.Yaml.Spec.DatadogFilter, after, 5000)

		log.Printf("Found records => %d \n", len(logs.Data))
		csvValues, err := y.getLogs(logs.Data)
		if err != nil {
			return
		}
		csv_processor.CsvWriter(y.CsvFile, nil, csvValues)

		if logs.Meta != nil && logs.Meta.Page != nil && logs.Meta.Page.After != nil {
			aft, ok := logs.Meta.Page.GetAfterOk()
			if ok {
				ddLogs(aft)
			}
		}

	}
	ddLogs(nil)
	return nil
}

// run the datadog downloader in parallel based on query util all logs are downloaded
func (y YamlProcessor) RunParallel(ch chan [][]string, done chan bool) {
	var headers []string
	util.ReadHeader(y.Yaml.Spec.Mapping, &headers)
	csv_processor.CsvWriter(y.CsvFile, headers, nil)

	var wg sync.WaitGroup
	intr := y.getInterval()

	for _, window := range intr {
		wg.Add(1)
		go func(win model.Interval, w *sync.WaitGroup, myChan chan [][]string) {
			defer w.Done()
			nddLogs = func(after *string, ddf model.DataDogFilter) {
				logs := dd.GetDataDogLogs(ddf, after, 5000)
				log.Printf("Found records => %d for date %d - %d \n", len(logs.Data), ddf.From, ddf.To)
				csvValues, err := y.getLogs(logs.Data)
				if err != nil {
					log.Fatalf(err.Error())
				}
				myChan <- csvValues
				if logs.Meta != nil && logs.Meta.Page != nil && logs.Meta.Page.After != nil {
					aft, ok := logs.Meta.Page.GetAfterOk()
					if ok {
						nddLogs(aft, ddf)
					}
				}
			}

			nddLogs(nil, model.DataDogFilter{
				Query: y.Yaml.Spec.DatadogFilter.Query,
				From:  win.From,
				To:    win.To,
			})
		}(window, &wg, ch)
	}
	wg.Wait()
	done <- true
}

// Match each log in datadog based on yaml mapping
// if field is an array we need search inside the array of structs
// one's found we need to get all values based on inner mapping
func (y YamlProcessor) getLogs(source []datadogV2.Log) ([][]string, error) {
	var csvValues [][]string
	for _, log_entry := range source {
		var ddVal []string
		// Custom: add each log attribute
		ddVal = append(ddVal, log_entry.Attributes.Timestamp.Format(time.RFC3339))
		ddVal = append(ddVal, *log_entry.Attributes.Service)
		ddVal = append(ddVal, *log_entry.Attributes.Status)
		ddVal = append(ddVal, fmt.Sprintf("\"%s\"", log_entry.Attributes.GetMessage()))
		attributes_json, err := json.Marshal(log_entry.Attributes.Attributes)
		if err != nil {
			log.Fatalf(err.Error())
		}
		ddVal = append(ddVal, fmt.Sprintf("'%s'", attributes_json))

		if log_entry.Attributes.HasMessage() {
			log_entry.Attributes.Attributes["message"] = fmt.Sprintf("\"%s\"", *log_entry.Attributes.Message)
		} else {
			log_entry.Attributes.Attributes["message"] = "\"\""
		}

		for _, fl := range y.Yaml.Spec.Mapping {
			if fl.Field == "-" {
				field_dep := strings.Split(fl.DdField, ".")
				n := len(field_dep)
				var outerObj = deepSearch(log_entry.Attributes.Attributes, field_dep)
				var newDdVal []string

				obj, ok := outerObj[field_dep[n-1]].([]interface{})
				if !ok {
					return nil, fmt.Errorf("not able to read value for '%s' field in %#v", field_dep[n-1], outerObj)
				}
				getAllValues(obj, fl.InnerField, &newDdVal)
				ddVal = append(ddVal, newDdVal...)
				continue
			}

			val := getValue(fl, log_entry.Attributes.Attributes)
			ddVal = append(ddVal, fmt.Sprint(val))
		}
		csvValues = append(csvValues, ddVal)
	}
	return csvValues, nil
}

func (y YamlProcessor) getInterval() []model.Interval {
	var interval []model.Interval
	from := y.Yaml.Spec.DatadogFilter.From
	to := y.Yaml.Spec.DatadogFilter.To

	diff_min := (to - from)
	if (diff_min / 60000) < 10 {
		interval = append(interval, model.Interval{
			From: from,
			To:   to,
		})
		return interval
	}

	each_interval := diff_min / 10

	for i := 0; i < 10; i++ {
		newInter := from + each_interval
		interval = append(interval, model.Interval{
			From: from,
			To:   newInter,
		})
		from = newInter
	}
	interval[9].To = to
	return interval
}
