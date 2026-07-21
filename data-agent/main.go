// data-agent — a GoFr 1.58 service that turns its OWN read endpoints into tools an
// AI agent can call. `app.EnableMCP()` exposes every GET handler as an MCP tool, and
// the /ask handler runs an agent loop with `ctx.LLM().Tools()` — the model answers
// natural-language questions by calling the service's real endpoints (auth,
// validation and observability all run, one coherent trace per request).
//
// The domain is a tiny in-memory storefront (products / orders / stats) so the whole
// thing runs with no database.
package main

import (
	"strings"

	"gofr.dev/pkg/gofr"
	"gofr.dev/pkg/gofr/ai"
)

const maxTurns = 6

type Product struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Price    float64 `json:"price"`
	Stock    int     `json:"stock"`
}

type Order struct {
	ID      string  `json:"id"`
	Product string  `json:"product_id"`
	Qty     int     `json:"qty"`
	Status  string  `json:"status"`
	Total   float64 `json:"total"`
}

var products = []Product{
	{"p1", "Aeron Chair", "furniture", 1395, 12},
	{"p2", "Standing Desk", "furniture", 640, 5},
	{"p3", "Mechanical Keyboard", "electronics", 130, 84},
	{"p4", "4K Monitor", "electronics", 410, 0},
	{"p5", "Desk Lamp", "lighting", 45, 210},
}

var orders = []Order{
	{"o1001", "p1", 2, "shipped", 2790},
	{"o1002", "p3", 5, "pending", 650},
	{"o1003", "p4", 1, "cancelled", 410},
	{"o1004", "p5", 10, "shipped", 450},
	{"o1005", "p2", 1, "pending", 640},
}

func main() {
	app := gofr.New()

	// Expose read handlers as MCP tools (loopback, GET-only). Enable up front so
	// routes registered afterwards are discovered too. MCP server on MCP_PORT (8200).
	app.EnableMCP()

	// These GET handlers become the agent's tools.
	app.GET("/products", listProducts)
	app.GET("/products/{id}", getProduct)
	app.GET("/orders", listOrders)
	app.GET("/stats", stats)

	// The agent loop over this service's own endpoints.
	app.POST("/ask", ask)

	app.Run()
}

func listProducts(c *gofr.Context) (any, error) {
	category := c.Param("category")
	if category == "" {
		return products, nil
	}

	out := make([]Product, 0, len(products))
	for _, p := range products {
		if strings.EqualFold(p.Category, category) {
			out = append(out, p)
		}
	}

	return out, nil
}

func getProduct(c *gofr.Context) (any, error) {
	id := c.PathParam("id")
	for _, p := range products {
		if p.ID == id {
			return p, nil
		}
	}

	return map[string]any{"found": false, "id": id}, nil
}

func listOrders(c *gofr.Context) (any, error) {
	status := c.Param("status")
	if status == "" {
		return orders, nil
	}

	out := make([]Order, 0, len(orders))
	for _, o := range orders {
		if strings.EqualFold(o.Status, status) {
			out = append(out, o)
		}
	}

	return out, nil
}

func stats(_ *gofr.Context) (any, error) {
	var revenue, pending float64
	byStatus := map[string]int{}

	for _, o := range orders {
		byStatus[o.Status]++
		if o.Status == "shipped" {
			revenue += o.Total
		}
		if o.Status == "pending" {
			pending += o.Total
		}
	}

	outOfStock := 0
	for _, p := range products {
		if p.Stock == 0 {
			outOfStock++
		}
	}

	return map[string]any{
		"shipped_revenue":     revenue,
		"pending_revenue":     pending,
		"orders_by_status":    byStatus,
		"products_out_of_stock": outOfStock,
		"product_count":       len(products),
	}, nil
}

// ask runs the agent loop: hand the model the service's own endpoints as tools,
// let it call them, feed results back, repeat until it answers.
func ask(c *gofr.Context) (any, error) {
	var in struct {
		Question string `json:"question"`
	}

	if err := c.Bind(&in); err != nil {
		return nil, err
	}

	model, tools := c.LLM(), c.LLM().Tools()

	messages := []ai.Message{
		{Role: ai.RoleSystem, Content: "You are a data assistant for an online storefront. " +
			"Answer questions about products, orders and revenue by CALLING the available tools to fetch live data — " +
			"never guess numbers. When you have enough data, give a short, direct answer."},
		{Role: ai.RoleUser, Content: in.Question},
	}

	for range maxTurns {
		resp, err := model.Chat(c, messages, ai.WithTools(tools.List()))
		if err != nil {
			return nil, err
		}

		if len(resp.ToolCalls) == 0 {
			return map[string]any{"answer": resp.Content}, nil
		}

		messages = append(messages, ai.Message{Role: ai.RoleAssistant, ToolCalls: resp.ToolCalls})

		for _, call := range resp.ToolCalls {
			result, err := tools.Call(c, call.Name, call.Args)
			if err != nil {
				messages = append(messages, ai.Message{Role: ai.RoleTool, ToolCallID: call.ID, Content: "error: " + err.Error()})
				continue
			}

			data, _ := result.JSON()
			messages = append(messages, ai.Message{Role: ai.RoleTool, ToolCallID: call.ID, Content: string(data)})
		}
	}

	return map[string]any{"answer": "agent did not converge within the turn budget"}, nil
}
