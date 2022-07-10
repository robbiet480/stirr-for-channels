package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/robbiet480/stirr-for-channels/internal/xmltv"
)

type StirrClient struct {
	StationID string

	httpClient http.Client

	Lineup       []Channel
	channels     []ChannelStatus
	ProgramCount int

	LastUpdate time.Time
	
	sync.Mutex
}

func NewStirrClient() (*StirrClient, error) {

	client := &StirrClient{
		httpClient: *http.DefaultClient,
	}

	client.StationID = os.Getenv("STIRR_STATION_ID")

	if client.StationID == "" {
		log.Println("STIRR_STATION_ID env var not set, attempting to auto detect local station")
		stationID, stationErr := client.GetStation()
		if stationErr != nil {
			return nil, stationErr
		}
		log.Println("Local station identified as", stationID)
		client.StationID = stationID
	}

	return client, nil
}

func (s *StirrClient) makeRequest(url string, output interface{}) error {
	if s.StationID != "" {
		url = fmt.Sprintf("%s?station=%s", url, s.StationID)
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	res, getErr := s.httpClient.Do(req)
	if getErr != nil {
		return getErr
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected response %v from %v", res.Status, url)
	}

	body, readErr := ioutil.ReadAll(res.Body)
	if readErr != nil {
		return readErr
	}

	return json.Unmarshal(body, &output)
}

func (s *StirrClient) GetStation() (string, error) {
	var autodetect StationDetection
	reqErr := s.makeRequest("https://ott-stationselection.sinclairstoryline.com/stationAutoSelection", &autodetect)
	return autodetect.Page[0].Button.MediaContent.SinclairActionConfig.Station[0], reqErr
}

func (s *StirrClient) GetChannels() ([]Channel, error) {
	var lineup Lineup
	reqErr := s.makeRequest("https://ott-gateway-stirr.sinclairstoryline.com/api/rest/v3/channels/stirr", &lineup)
	return lineup.Channels, reqErr
}

func (s *StirrClient) GetChannel(channelID string) (*ChannelStatus, error) {
	status := ChannelStatus{}
	reqErr := s.makeRequest(fmt.Sprintf("https://ott-gateway-stirr.sinclairstoryline.com/api/rest/v3/status/%s", channelID), &status)
	return &status, reqErr
}

func (s *StirrClient) GetChannelPrograms(channelID string) ([]Program, error) {
	guide := GuideData{}
	reqErr := s.makeRequest(fmt.Sprintf("https://ott-gateway-stirr.sinclairstoryline.com/api/rest/v3/program/stirr/ott/%s", channelID), &guide)
	return guide.Programs, reqErr
}

func (s *StirrClient) FillCache() error {
	log.Println("Beginning cache fill")
	lineup, lineupErr := s.GetChannels()
	if lineupErr != nil {
		return lineupErr
	}

	s.Lock()
	defer s.Unlock()

	s.Lineup = lineup
	s.channels = nil

	log.Println("Found", len(lineup), "channels in lineup, getting channel metadata and guide. This may take a moment.")

	totalPrograms := 0

	for idx, channel := range lineup {
		fmt.Print(".")
		status, statusErr := s.GetChannel(channel.DisplayName)
		if statusErr != nil {
			log.Println("Ignoring error on", channel.DisplayName, ":", statusErr)
			return nil
		}
		status.Number = idx + 1

		status.ID = fmt.Sprintf("stirr-%s", channel.ID)

		programs, programsErr := s.GetChannelPrograms(channel.DisplayName)
		if programsErr != nil {
			return programsErr
		}

		totalPrograms = totalPrograms + len(programs)

		status.Programs = programs

		s.channels = append(s.channels, *status)
	}
	fmt.Println()
	log.Println("Cache fill complete, loaded", len(lineup), "channels with", totalPrograms, "programs in guide")

	s.LastUpdate = time.Now()
	s.ProgramCount = totalPrograms

	sort.Slice(s.channels, func(i, j int) bool { return s.channels[i].Number < s.channels[j].Number })

	return nil
}

func playlist(client *StirrClient) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "audio/x-mpegurl")

		allLines := []string{}
		
		client.Lock()
		defer client.Unlock()

		for _, channel := range client.channels {
			allLines = append(allLines, channel.M3ULine())
		}

		fmt.Fprintf(w, "#EXTM3U\n%s\n", strings.Join(allLines, "\n\n"))
	}
}

func epg(client *StirrClient) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/xml")

		epg := &xmltv.TV{
			GeneratorInfoName: "stirr-for-channels",
			GeneratorInfoURL:  "https://github.com/robbiet480/stirr-for-channels",
		}

		client.Lock()
		defer client.Unlock()

		for _, channel := range client.channels {
			epg.Channels = append(epg.Channels, channel.XMLTV())

			for _, program := range channel.Programs {
				epg.Programmes = append(epg.Programmes, program.XMLTV())
			}
		}

		buf, marshallErr := xml.MarshalIndent(epg, "", "\t")
		if marshallErr != nil {
			log.Fatalln("error marshalling EPG to XML", marshallErr)
		}
		w.Write([]byte(xml.Header + `<!DOCTYPE tv SYSTEM "xmltv.dtd">` + "\n" + string(buf)))
	}
}

func index(client *StirrClient) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html")

		tmpl, tmplErr := template.New("idx").Parse(`<!DOCTYPE html>
		<html>
			<head>
				<meta charset="utf-8">
				<meta name="viewport" content="width=device-width, initial-scale=1">
				<title>Stirr for Channels</title>
				<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/bulma@0.9.1/css/bulma.min.css">
				<style>
					ul{
						margin-bottom: 10px;
					}
				</style>
			</head>
			<body>
			<section class="section">
				<div class="container">
					<h1 class="title">
						Stirr for Channels
					</h1>
					<p class="subtitle">
						Station ID: {{ .StationID }}<br>
						Channel count: {{ len .Lineup }}<br>
						Program count: {{ .ProgramCount }}<br>
						Last Updated: {{ .LastUpdate.Format "Mon Jan _2 15:04:05 MST 2006" }}
					</p>
					<ul>
						<li><a href="/playlist.m3u">Playlist</a></li>
						<li><a href="/epg.xml">EPG</a></li>
					</ul>
					<ul>
						<li><a href="https://github.com/robbiet480/stirr-for-channels">Source Code</a></li>
					</ul>
				</div>
			</section>
			</body>
		</html>`)

		if tmplErr != nil {
			log.Fatalln("Error when building index HTML from template", tmplErr)
		}

		if execErr := tmpl.Execute(w, client); execErr != nil {
			log.Fatalln("Error when rendering index HTML", execErr)
		}
	}
}

func main() {
	client, clientErr := NewStirrClient()

	if clientErr != nil {
		log.Fatalln("Error when configuring Stirr client", clientErr)
	}

	if fillErr := client.FillCache(); fillErr != nil {
		log.Fatalln("Error when filling cache", fillErr)
	}

	ticker := time.NewTicker(30 * time.Minute)
	quit := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				if fillErr := client.FillCache(); fillErr != nil {
					log.Fatalln("Error when filling cache", fillErr)
				}
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()

	http.HandleFunc("/playlist.m3u", playlist(client))
	http.HandleFunc("/epg.xml", epg(client))
	http.HandleFunc("/", index(client))

	port := os.Getenv("PORT")
	if port == "" {
		port = "80"
	}

	log.Println("Now accepting requests on :" + port)

	if httpErr := http.ListenAndServe(":"+port, nil); httpErr != nil {
		log.Fatalln("Error starting server", httpErr)
	}

}
