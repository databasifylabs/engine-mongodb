package mongodb

import (
	"cmp"
	"context"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/databasifylabs/engine-sdk/dtos"
	"github.com/databasifylabs/engine-sdk/responses"
)

const topStatements = 50

type profileGroup struct {
	Calls   int64  `bson:"calls"`
	TotalMs int64  `bson:"totalMs"`
	Ns      string `bson:"ns"`
	Op      string `bson:"op"`
	Filter  bson.D `bson:"filter"`
}

var profilePipeline = bson.A{
	bson.D{{Key: "$group", Value: bson.D{
		{Key: "_id", Value: bson.D{{Key: "$ifNull", Value: bson.A{"$queryHash", "$planCacheShapeHash", bson.D{{Key: "$concat", Value: bson.A{"$ns", " ", "$op"}}}}}}},
		{Key: "calls", Value: bson.D{{Key: "$sum", Value: 1}}},
		{Key: "totalMs", Value: bson.D{{Key: "$sum", Value: "$millis"}}},
		{Key: "ns", Value: bson.D{{Key: "$first", Value: "$ns"}}},
		{Key: "op", Value: bson.D{{Key: "$first", Value: "$op"}}},
		{Key: "filter", Value: bson.D{{Key: "$first", Value: "$command.filter"}}},
	}}},
	bson.D{{Key: "$sort", Value: bson.D{{Key: "totalMs", Value: -1}}}},
	bson.D{{Key: "$limit", Value: topStatements}},
}

func (e nodes) Insights(ctx context.Context) (responses.QueryInsights, error) {
	var listed struct {
		Databases []struct {
			Name string `bson:"name"`
		} `bson:"databases"`
	}
	if err := e.admin.Run(ctx, "admin", bson.D{{Key: "listDatabases", Value: 1}, {Key: "nameOnly", Value: true}}, &listed); err != nil {
		return responses.QueryInsights{}, err
	}
	groups := []profileGroup{}
	for _, database := range listed.Databases {
		if slices.Contains(e.Capabilities().ReservedDatabases, database.Name) {
			continue
		}
		var reply struct {
			Cursor struct {
				FirstBatch []profileGroup `bson:"firstBatch"`
			} `bson:"cursor"`
		}
		cmd := bson.D{{Key: "aggregate", Value: "system.profile"}, {Key: "pipeline", Value: profilePipeline}, {Key: "cursor", Value: bson.D{{Key: "batchSize", Value: topStatements}}}}
		if err := e.admin.Run(ctx, database.Name, cmd, &reply); err != nil {
			return responses.QueryInsights{}, err
		}
		groups = append(groups, reply.Cursor.FirstBatch...)
	}

	return responses.QueryInsights{Statements: statementsOf(groups)}, nil
}

func statementsOf(groups []profileGroup) []dtos.Statement {
	slices.SortFunc(groups, func(left, right profileGroup) int { return cmp.Compare(right.TotalMs, left.TotalMs) })
	statements := []dtos.Statement{}
	for _, group := range groups[:min(len(groups), topStatements)] {
		statements = append(statements, dtos.Statement{Text: textOf(group), Calls: group.Calls, TotalMs: group.TotalMs, MeanMs: group.TotalMs / max(group.Calls, 1)})
	}

	return statements
}

func textOf(group profileGroup) string {
	text := group.Ns + " " + group.Op
	if group.Filter == nil {
		return text
	}
	shape, err := bson.MarshalExtJSON(shapeOf(group.Filter), false, false)
	if err != nil {
		return text
	}

	return text + " " + string(shape)
}

func shapeOf(value any) any {
	document, isDocument := value.(bson.D)
	if !isDocument {
		return "?"
	}
	shaped := bson.D{}
	for _, element := range document {
		shaped = append(shaped, bson.E{Key: element.Key, Value: shapeOf(element.Value)})
	}

	return shaped
}
