package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/hasura/go-graphql-client"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

type AppEventType string // RELATIONSHIP_INSTALLED,RELATIONSHIP_DEACTIVATED,RELATIONSHIP_REACTIVATED,RELATIONSHIP_UNINSTALLED

type AppEventNode struct {
	Type       AppEventType `json:"type"`
	OccurredAt time.Time    `json:"occurredAt"`
}

type AppEventEdges struct {
	Node   AppEventNode `json:"node"`
	Cursor string       `json:"cursor"`
}

type PageInfo struct {
	HasNextPage bool `json:"hasNextPage"`
}

type AppEvent struct {
	Edges    []AppEventEdges `json:"edges"`
	PageInfo PageInfo        `json:"pageInfo"`
}

type App struct {
	AppEvent AppEvent `json:"events" graphql:"events(first: 100,after:$endCursor,occurredAtMin:$occurredAtMin)"`
	Name     string   `json:"name"`
}

type AppLite struct {
	Name string `json:"name"`
}

type AppEventStatistic map[AppEventType]int

type AppEventStatisticByDate map[string]AppEventStatistic

func getAppEventStatistic(edges []AppEventEdges) AppEventStatisticByDate {
	appEventStatistic := make(AppEventStatisticByDate)
	for _, edge := range edges {
		// Convert UTC to GMT+7
		localTime := edge.Node.OccurredAt.In(time.FixedZone("GMT+7", 7*60*60))
		date := localTime.Format("2006-01-02")
		fmt.Printf("processing date: %s, type: %s\n", date, edge.Node.Type)
		if _, ok := appEventStatistic[date]; !ok {
			appEventStatistic[date] = make(AppEventStatistic)
		}
		appEventStatistic[date][edge.Node.Type]++
	}
	return appEventStatistic
}

type DateTime string

func (c *DateTime) GetGraphQLType() string { return "DateTime" }

var HumanReadableEventType = map[AppEventType]string{
	AppEventType("RELATIONSHIP_INSTALLED"):   "Installs",
	AppEventType("RELATIONSHIP_DEACTIVATED"): "Closed",
	AppEventType("RELATIONSHIP_REACTIVATED"): "Reopened",
	AppEventType("RELATIONSHIP_UNINSTALLED"): "Uninstalls",
}

func main() {
	// retrieve partner token from args
	partnerToken := os.Args[1]
	appId := os.Args[2]
	partnerId := os.Args[3]
	now := time.Now()
	gmt7 := time.FixedZone("GMT+7", 7*60*60)
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, gmt7).UTC().Format(time.RFC3339)
	if len(os.Args) > 4 {
		// format 2024-05-01
		timeString := os.Args[4] + "T00:00:00+07:00"
		t, err := time.Parse(time.RFC3339, timeString)
		if err != nil {
			log.Fatal(err)
		}
		startOfDay = t.UTC().Format(time.RFC3339)
	}

	occurredAtMin := DateTime(startOfDay)
	occurredAtMax := DateTime(time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 999999999, gmt7).UTC().Format(time.RFC3339))
	fmt.Printf("startOfDay: %s, endOfDay: %s\n", occurredAtMin, occurredAtMax)
	client := graphql.NewClient(fmt.Sprintf("https://partners.shopify.com/%s/api/2024-07/graphql.json", partnerId), nil)
	client = client.WithRequestModifier(func(r *http.Request) {
		// transtore partner token
		r.Header.Add("X-Shopify-Access-Token", partnerToken)
	}).WithDebug(true)

	// query app name for csv file name
	var appQuery struct {
		App AppLite `graphql:"app(id: $appId)"`
	}
	err := client.Query(context.Background(), &appQuery, map[string]any{
		"appId": graphql.ID(appId),
	})
	if err != nil {
		log.Fatal(err)
	}

	var allEdges []AppEventEdges
	var endCursor *string
	hasNextPage := true

	for hasNextPage {
		var query struct {
			App App `graphql:"app(id: $appId)"`
		}

		err := client.Query(context.Background(), &query, map[string]any{
			"appId":         graphql.ID(appId),
			"endCursor":     endCursor,
			"occurredAtMin": occurredAtMin,
		})
		log.Printf("query app events, occurredAtMin: %s, endCursor: %v", occurredAtMin, endCursor)
		if err != nil {
			log.Printf("Error querying GraphQL: %v", err)
			break
		}

		allEdges = append(allEdges, query.App.AppEvent.Edges...)
		if !query.App.AppEvent.PageInfo.HasNextPage {
			hasNextPage = false
		} else if len(query.App.AppEvent.Edges) > 0 {
			lastEdge := query.App.AppEvent.Edges[len(query.App.AppEvent.Edges)-1]
			endCursor = &lastEdge.Cursor
		}
	}

	// get app event statistic
	appEventStatistic := getAppEventStatistic(allEdges)

	// Extract and sort dates
	dates := make([]string, 0, len(appEventStatistic))
	for date := range appEventStatistic {
		dates = append(dates, date)
	}
	sort.Strings(dates)

	// Use sorted dates to write CSV
	// f, err := os.Create(fmt.Sprintf("%s_event_statistic.csv", appQuery.App.Name))
	// if err != nil {
	// 	log.Fatal(err)
	// }
	// defer f.Close()

	// f.WriteString("Date,Installs,Closed,Reopened,Uninstalls\n")
	// for _, date := range dates {
	// 	eventStatistic := appEventStatistic[date]
	// 	f.WriteString(fmt.Sprintf("%s,%d,%d,%d,%d\n", date,
	// 		eventStatistic[AppEventType("RELATIONSHIP_INSTALLED")],
	// 		eventStatistic[AppEventType("RELATIONSHIP_DEACTIVATED")],
	// 		eventStatistic[AppEventType("RELATIONSHIP_REACTIVATED")],
	// 		eventStatistic[AppEventType("RELATIONSHIP_UNINSTALLED")]))
	// }
	// fmt.Println("CSV file created successfully")

	// Write CSV content to Google Sheets
	err = writeToGoogleSheets(appQuery.App.Name, dates, appEventStatistic)
	if err != nil {
		log.Fatalf("Failed to write to Google Sheets: %v", err)
	}
	fmt.Println("Data written to Google Sheets successfully")
}

func writeToGoogleSheets(sheetName string, dates []string, appEventStatistic AppEventStatisticByDate) error {
	ctx := context.Background()
	srv, err := sheets.NewService(ctx, option.WithCredentialsFile("credentials.json"))
	if err != nil {
		return fmt.Errorf("unable to retrieve Sheets client: %v", err)
	}

	spreadsheetId := "13W9lf1gZY3haffKXMApebmN8RqQQPTGwsA7eISf-Rnc"

	// Check if the sheet already exists
	spreadsheet, err := srv.Spreadsheets.Get(spreadsheetId).Do()
	if err != nil {
		return fmt.Errorf("unable to retrieve spreadsheet: %v", err)
	}

	sheetExists := false
	for _, sheet := range spreadsheet.Sheets {
		if sheet.Properties.Title == sheetName {
			sheetExists = true
			break
		}
	}

	// If the sheet doesn't exist, create it
	if !sheetExists {
		createRequest := sheets.Request{
			AddSheet: &sheets.AddSheetRequest{
				Properties: &sheets.SheetProperties{
					Title: sheetName,
				},
			},
		}

		batchUpdateRequest := &sheets.BatchUpdateSpreadsheetRequest{
			Requests: []*sheets.Request{&createRequest},
		}

		_, err := srv.Spreadsheets.BatchUpdate(spreadsheetId, batchUpdateRequest).Do()
		if err != nil {
			return fmt.Errorf("unable to create new sheet: %v", err)
		}
	}

	// Check if header row exists
	headerRange, err := srv.Spreadsheets.Values.Get(spreadsheetId, sheetName+"!A1:E1").Do()
	if err != nil {
		return fmt.Errorf("unable to retrieve header row: %v", err)
	}

	// Get the current data in the sheet
	readRange := sheetName + "!A:E"
	existingData, err := srv.Spreadsheets.Values.Get(spreadsheetId, readRange).Do()
	if err != nil {
		return fmt.Errorf("unable to retrieve existing data: %v", err)
	}

	var values [][]interface{}
	nextRow := 1
	dateRowMap := make(map[string]int)

	if len(headerRange.Values) == 0 || !headerRowMatches(headerRange.Values[0]) {
		// Add header row if it doesn't exist or doesn't match
		_, err = srv.Spreadsheets.Values.Update(spreadsheetId, sheetName+"!A1:E1", &sheets.ValueRange{
			Values: [][]interface{}{{"Date", "Installs", "Closed", "Reopened", "Uninstalls"}},
		}).ValueInputOption("RAW").Do()
		if err != nil {
			return fmt.Errorf("unable to update header row: %v", err)
		}
		nextRow = 2 // Start data from the second row
	} else {
		nextRow = len(existingData.Values) + 1
	}

	// Create a map of existing dates and their row numbers
	for i, row := range existingData.Values {
		if len(row) > 0 {
			if date, ok := row[0].(string); ok {
				dateRowMap[date] = i + 1 // +1 because sheet rows are 1-indexed
			}
		}
	}

	// Process new data
	for _, date := range dates {
		eventStatistic := appEventStatistic[date]
		rowData := []interface{}{
			date,
			eventStatistic[AppEventType("RELATIONSHIP_INSTALLED")],
			eventStatistic[AppEventType("RELATIONSHIP_DEACTIVATED")],
			eventStatistic[AppEventType("RELATIONSHIP_REACTIVATED")],
			eventStatistic[AppEventType("RELATIONSHIP_UNINSTALLED")],
		}

		if existingRow, found := dateRowMap[date]; found {
			// Update existing row
			updateRange := fmt.Sprintf("%s!A%d:E%d", sheetName, existingRow, existingRow)
			updateValueRange := &sheets.ValueRange{
				Values: [][]interface{}{rowData},
			}
			_, err = srv.Spreadsheets.Values.Update(spreadsheetId, updateRange, updateValueRange).
				ValueInputOption("RAW").Do()
			if err != nil {
				return fmt.Errorf("unable to update existing row: %v", err)
			}
		} else {
			// Append new row
			values = append(values, rowData)
		}
	}

	// Append new rows if any
	if len(values) > 0 {
		appendRange := fmt.Sprintf("%s!A%d", sheetName, nextRow)
		appendValueRange := &sheets.ValueRange{
			Values: values,
		}
		_, err = srv.Spreadsheets.Values.Append(spreadsheetId, appendRange, appendValueRange).
			ValueInputOption("RAW").Do()
		if err != nil {
			return fmt.Errorf("unable to append new rows: %v", err)
		}
	}

	return nil
}

// Helper function to check if the header row matches our expected headers
func headerRowMatches(row []interface{}) bool {
	expectedHeaders := []string{"Date", "Installs", "Closed", "Reopened", "Uninstalls"}
	if len(row) != len(expectedHeaders) {
		return false
	}
	for i, header := range row {
		if header != expectedHeaders[i] {
			return false
		}
	}
	return true
}
