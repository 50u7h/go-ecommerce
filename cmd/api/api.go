package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/go-chi/chi/v5"
	"github.com/stripe/stripe-go/v75"
	"goEcommerce/internal/cards"
	"goEcommerce/internal/driver"
	"goEcommerce/internal/encryption"
	"goEcommerce/internal/models"
	"goEcommerce/internal/urlsigner"
	"goEcommerce/internal/validator"
	"golang.org/x/crypto/bcrypt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const version = "1.0.0"

type config struct {
	port int
	env  string
	db   struct {
		dsn string
	}
	stripe struct {
		secret string
		key    string
	}
	smtp struct {
		host     string
		port     int
		username string
		password string
	}
	secretkey string
	frontend  string
}

type application struct {
	config   config
	infoLog  *log.Logger
	errorLog *log.Logger
	version  string
	DB       models.DBModel
}

func (app *application) serve() error {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", app.config.port),
		Handler:           app.routes(),
		IdleTimeout:       30 * time.Second,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}

	app.infoLog.Printf("Starting Back end server in %s mode on port %d\n", app.config.env, app.config.port)

	return srv.ListenAndServe()
}

func main() {
	var cfg config

	flag.IntVar(&cfg.port, "port", 4001, "Server port to listen on")
	flag.StringVar(&cfg.env, "env", "development", "Application environment {development|production|maintenance}")
	flag.StringVar(&cfg.db.dsn, "dsn", "username:password@tcp(localhost:3306)/widgets?parseTime=true&tls=false", "DSN")
	flag.StringVar(&cfg.smtp.host, "smtphost", "sandbox.smtp.mailtrap.io", "smtp host")
	flag.StringVar(&cfg.smtp.username, "smtpuser", "406ff5e32131ed", "smtp user")
	flag.StringVar(&cfg.smtp.password, "smtppass", "3ba16b17d0d4f1", "smtp password")
	flag.IntVar(&cfg.smtp.port, "smtpport", 587, "smtp port")
	flag.StringVar(&cfg.secretkey, "secret", "qdYaJw3sIhTVH5opBEr0PNoIXLWr5QqC", "secret key")
	flag.StringVar(&cfg.frontend, "frontend", "http://localhost:4000", "url to front end")

	flag.Parse()

	cfg.stripe.key = os.Getenv("STRIPE_KEY")
	cfg.stripe.secret = os.Getenv("STRIPE_SECRET")

	infoLog := log.New(os.Stdout, "INFO\t", log.Ldate|log.Ltime)
	errorLog := log.New(os.Stdout, "ERROR\t", log.Ldate|log.Ltime|log.Lshortfile)

	conn, err := driver.OpenDB(cfg.db.dsn)
	if err != nil {
		errorLog.Fatal(err)
	}
	defer func(conn *sql.DB) {
		err := conn.Close()
		if err != nil {
			return
		}
	}(conn)

	app := &application{
		config:   cfg,
		infoLog:  infoLog,
		errorLog: errorLog,
		version:  version,
		DB:       models.DBModel{DB: conn},
	}

	err = app.serve()
	if err != nil {
		log.Fatal(err)
	}
}

// GetPaymentIntent gets a payment intent, and returns json (or error json)
func (app *application) GetPaymentIntent(w http.ResponseWriter, r *http.Request) {
	var payload stripePayload

	err := json.NewDecoder(r.Body).Decode(&payload)
	if err != nil {
		app.errorLog.Println(err)
		return
	}

	amount, err := strconv.Atoi(payload.Amount)
	if err != nil {
		app.errorLog.Println(err)
		return
	}

	card := cards.Card{
		Secret:   app.config.stripe.secret,
		Key:      app.config.stripe.key,
		Currency: payload.Currency,
	}

	okay := true

	pi, msg, err := card.Charge(payload.Currency, amount)
	if err != nil {
		okay = false
	}

	if okay {
		out, err := json.MarshalIndent(pi, "", "   ")
		if err != nil {
			app.errorLog.Println(err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(out)
		if err != nil {
			return
		}
	} else {
		j := jsonResponse{
			OK:      false,
			Message: msg,
			Content: "",
		}

		out, err := json.MarshalIndent(j, "", "   ")
		if err != nil {
			app.errorLog.Println(err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(out)
		if err != nil {
			return
		}
	}
}

// GetWidgetByID gets one widget by id and returns as JSON
func (app *application) GetWidgetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	widgetID, _ := strconv.Atoi(id)

	widget, err := app.DB.GetWidget(widgetID)
	if err != nil {
		app.errorLog.Println(err)
		return
	}

	out, err := json.MarshalIndent(widget, "", "   ")
	if err != nil {
		app.errorLog.Println(err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(out)
	if err != nil {
		return
	}
}

// Invoice describes the JSON payload sent to the microservice
type Invoice struct {
	ID        int       `json:"id"`
	WidgetID  int       `json:"widget_id"`
	Amount    int       `json:"amount"`
	Product   string    `json:"product"`
	Quantity  int       `json:"quantity"`
	FirstName string    `json:"first_name"`
	LastName  string    `json:"last_name"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateCustomerAndSubscribeToPlan is the handler for subscribing to the bronze plan
func (app *application) CreateCustomerAndSubscribeToPlan(w http.ResponseWriter, r *http.Request) {
	var data stripePayload
	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		app.errorLog.Println(err)
		return
	}

	// validate data
	v := validator.New()
	v.Check(len(data.FirstName) > 1, "first_name", "must be at least 2 characters")

	if !v.Valid() {
		app.failedValidation(w, r, v.Errors)
		return
	}

	card := cards.Card{
		Secret:   app.config.stripe.secret,
		Key:      app.config.stripe.key,
		Currency: data.Currency,
	}

	okay := true
	var subscription *stripe.Subscription
	txnMsg := "Transaction successful"

	stripeCustomer, msg, err := card.CreateCustomer(data.PaymentMethod, data.Email)
	if err != nil {
		app.errorLog.Println(err)
		okay = false
		txnMsg = msg
	}

	if okay {
		subscription, err = card.SubscribeToPlan(stripeCustomer, data.Plan, data.Email, data.LastFour, "")
		if err != nil {
			app.errorLog.Println(err)
			okay = false
			txnMsg = "Error subscribing customer"
		}
	}

	if okay {
		productID, _ := strconv.Atoi(data.ProductID)
		customerID, err := app.SaveCustomer(data.FirstName, data.LastName, data.Email)
		if err != nil {
			app.errorLog.Println(err)
			return
		}

		// create a new txn
		amount, _ := strconv.Atoi(data.Amount)
		txn := models.Transaction{
			Amount:              amount,
			Currency:            "usd",
			LastFour:            data.LastFour,
			ExpiryMonth:         data.ExpiryMonth,
			ExpiryYear:          data.ExpiryYear,
			TransactionStatusID: 2,
			PaymentIntent:       subscription.ID,
			PaymentMethod:       data.PaymentMethod,
		}

		txnID, err := app.SaveTransaction(txn)
		if err != nil {
			app.errorLog.Println(err)
			return
		}

		// create order
		order := models.Order{
			WidgetID:      productID,
			TransactionID: txnID,
			CustomerID:    customerID,
			StatusID:      1,
			Quantity:      1,
			Amount:        amount,
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
		}

		orderID, err := app.SaveOrder(order)
		if err != nil {
			app.errorLog.Println(err)
			return
		}

		inv := Invoice{
			ID:        orderID,
			Amount:    2000,
			Product:   "Bronze Plan monthly subscription",
			Quantity:  order.Quantity,
			FirstName: data.FirstName,
			LastName:  data.LastName,
			Email:     data.Email,
			CreatedAt: time.Now(),
		}

		err = app.callInvoiceMicro(inv)
		if err != nil {
			app.errorLog.Println(err)
		}
	}

	resp := jsonResponse{
		OK:      okay,
		Message: txnMsg,
	}

	out, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		app.errorLog.Println(err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// callInvoiceMicro calls the invoicing microservice
func (app *application) callInvoiceMicro(inv Invoice) error {
	url := "http://localhost:5000/invoice/create-and-send"
	out, err := json.MarshalIndent(inv, "", "\t")
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(out))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {

		}
	}(resp.Body)

	return nil
}

// SaveCustomer saves a customer and returns id
func (app *application) SaveCustomer(firstName, lastName, email string) (int, error) {
	customer := models.Customer{
		FirstName: firstName,
		LastName:  lastName,
		Email:     email,
	}

	id, err := app.DB.InsertCustomer(customer)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// SaveTransaction saves a txn and returns id
func (app *application) SaveTransaction(txn models.Transaction) (int, error) {
	id, err := app.DB.InsertTransaction(txn)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// SaveOrder saves a order and returns id
func (app *application) SaveOrder(order models.Order) (int, error) {
	id, err := app.DB.InsertOrder(order)
	if err != nil {
		return 0, err
	}
	return id, nil
}

// authenticateToken checks an auth token for validity
func (app *application) CreateAuthToken(w http.ResponseWriter, r *http.Request) {
	var userInput struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	err := app.readJSON(w, r, &userInput)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	// get the user from the database by email; send error if invalid email
	user, err := app.DB.GetUserByEmail(userInput.Email)
	if err != nil {
		err := app.invalidCredentials(w)
		if err != nil {
			return
		}
		return
	}

	// validate the password; send error if invalid password
	validPassword, err := app.passwordMatches(user.Password, userInput.Password)
	if err != nil {
		err := app.invalidCredentials(w)
		if err != nil {
			return
		}
		return
	}

	if !validPassword {
		err := app.invalidCredentials(w)
		if err != nil {
			return
		}
		return
	}

	// generate the token
	token, err := models.GenerateToken(user.ID, time.Hour*24, models.ScopeAuthentication)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	// save to database
	err = app.DB.InsertToken(token, user)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	// send response
	var payload struct {
		Error   bool          `json:"error"`
		Message string        `json:"message"`
		Token   *models.Token `json:"authentication_token"`
	}
	payload.Error = false
	payload.Message = fmt.Sprintf("token for %s created", userInput.Email)
	payload.Token = token

	_ = app.writeJSON(w, http.StatusOK, payload)

}

// CheckAuthentication checks auth status
func (app *application) CheckAuthentication(w http.ResponseWriter, r *http.Request) {
	// validate the token, and get associated user
	user, err := app.authenticateToken(r)
	if err != nil {
		err := app.invalidCredentials(w)
		if err != nil {
			return
		}
		return
	}

	// valid user
	var payload struct {
		Error   bool   `json:"error"`
		Message string `json:"message"`
	}
	payload.Error = false
	payload.Message = fmt.Sprintf("authenticated user %s", user.Email)

	err = app.writeJSON(w, http.StatusOK, payload)
	if err != nil {
		return
	}
}

// VirtualTerminalPaymentSucceeded displays a page with receipt information
func (app *application) authenticateToken(r *http.Request) (*models.User, error) {
	authorizationHeader := r.Header.Get("Authorization")
	if authorizationHeader == "" {
		return nil, errors.New("no authorization header received")
	}

	headerParts := strings.Split(authorizationHeader, " ")
	if len(headerParts) != 2 || headerParts[0] != "Bearer" {
		return nil, errors.New("no authorization header received")
	}

	token := headerParts[1]
	if len(token) != 26 {
		return nil, errors.New("authentication token wrong size")
	}

	// get the user from the tokens table
	user, err := app.DB.GetUserForToken(token)
	if err != nil {
		return nil, errors.New("no matching user found")
	}

	return user, nil
}

func (app *application) VirtualTerminalPaymentSucceeded(w http.ResponseWriter, r *http.Request) {
	var txnData struct {
		PaymentAmount   int    `json:"amount"`
		PaymentCurrency string `json:"currency"`
		FirstName       string `json:"first_name"`
		LastName        string `json:"last_name"`
		Email           string `json:"email"`
		PaymentIntent   string `json:"payment_intent"`
		PaymentMethod   string `json:"payment_method"`
		BankReturnCode  string `json:"bank_return_code"`
		ExpiryMonth     int    `json:"expiry_month"`
		ExpiryYear      int    `json:"expiry_year"`
		LastFour        string `json:"last_four"`
	}

	err := app.readJSON(w, r, &txnData)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	card := cards.Card{
		Secret: app.config.stripe.secret,
		Key:    app.config.stripe.key,
	}

	pi, err := card.RetrievePaymentIntent(txnData.PaymentIntent)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	pm, err := card.GetPaymentMethod(txnData.PaymentMethod)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	txnData.LastFour = pm.Card.Last4
	txnData.ExpiryMonth = int(pm.Card.ExpMonth)
	txnData.ExpiryYear = int(pm.Card.ExpYear)

	txn := models.Transaction{
		Amount:              txnData.PaymentAmount,
		Currency:            txnData.PaymentCurrency,
		LastFour:            txnData.LastFour,
		ExpiryMonth:         txnData.ExpiryMonth,
		ExpiryYear:          txnData.ExpiryYear,
		PaymentIntent:       txnData.PaymentIntent,
		PaymentMethod:       txnData.PaymentMethod,
		BankReturnCode:      pi.LatestCharge.ID,
		TransactionStatusID: 2,
	}

	_, err = app.SaveTransaction(txn)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	err = app.writeJSON(w, http.StatusOK, txn)
	if err != nil {
		return
	}
}

// SendPasswordResetEmail sends an email with a signed url to allow user to reset password
func (app *application) SendPasswordResetEmail(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Email string `json:"email"`
	}

	err := app.readJSON(w, r, &payload)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	// verify that email exists
	_, err = app.DB.GetUserByEmail(payload.Email)
	if err != nil {
		var resp struct {
			Error   bool   `json:"error"`
			Message string `json:"message"`
		}
		resp.Error = true
		resp.Message = "No matching email found on system"
		err := app.writeJSON(w, http.StatusAccepted, resp)
		if err != nil {
			return
		}
		return
	}

	link := fmt.Sprintf("%s/reset-password?email=%s", app.config.frontend, payload.Email)

	sign := urlsigner.Signer{
		Secret: []byte(app.config.secretkey),
	}

	signedLink := sign.GenerateTokenFromString(link)

	var data struct {
		Link string
	}

	data.Link = signedLink

	// send mail
	err = app.SendMail("info@south.com", payload.Email, "Password Reset Request", "password-reset", data)
	if err != nil {
		app.errorLog.Println(err)
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	var resp struct {
		Error   bool   `json:"error"`
		Message string `json:"message"`
	}

	resp.Error = false

	err = app.writeJSON(w, http.StatusCreated, resp)
	if err != nil {
		return
	}
}

// ResetPassword resets a user's password in the database
func (app *application) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	err := app.readJSON(w, r, &payload)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	encryptor := encryption.Encryption{
		Key: []byte(app.config.secretkey),
	}

	realEmail, err := encryptor.Decrypt(payload.Email)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	user, err := app.DB.GetUserByEmail(realEmail)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(payload.Password), 12)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	err = app.DB.UpdatePasswordForUser(user, string(newHash))
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	var resp struct {
		Error   bool   `json:"error"`
		Message string `json:"message"`
	}

	resp.Error = false
	resp.Message = "password changed"

	err = app.writeJSON(w, http.StatusCreated, resp)
	if err != nil {
		return
	}
}

// AllSales returns all sales as a slice
func (app *application) AllSales(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		PageSize    int `json:"page_size"`
		CurrentPage int `json:"page"`
	}

	err := app.readJSON(w, r, &payload)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	allSales, lastPage, totalRecords, err := app.DB.GetAllOrdersPaginated(payload.PageSize, payload.CurrentPage)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	var resp struct {
		CurrentPage  int             `json:"current_page"`
		PageSize     int             `json:"page_size"`
		LastPage     int             `json:"last_page"`
		TotalRecords int             `json:"total_records"`
		Orders       []*models.Order `json:"orders"`
	}

	resp.CurrentPage = payload.CurrentPage
	resp.PageSize = payload.PageSize
	resp.LastPage = lastPage
	resp.TotalRecords = totalRecords
	resp.Orders = allSales

	err = app.writeJSON(w, http.StatusOK, resp)
	if err != nil {
		return
	}
}

// AllSubscriptions returns all subscriptions as a slice
func (app *application) AllSubscriptions(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		PageSize    int `json:"page_size"`
		CurrentPage int `json:"page"`
	}

	err := app.readJSON(w, r, &payload)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	allSales, lastPage, totalRecords, err := app.DB.GetAllSubscriptionsPaginated(payload.PageSize, payload.CurrentPage)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	var resp struct {
		CurrentPage  int             `json:"current_page"`
		PageSize     int             `json:"page_size"`
		LastPage     int             `json:"last_page"`
		TotalRecords int             `json:"total_records"`
		Orders       []*models.Order `json:"orders"`
	}

	resp.CurrentPage = payload.CurrentPage
	resp.PageSize = payload.PageSize
	resp.LastPage = lastPage
	resp.TotalRecords = totalRecords
	resp.Orders = allSales

	err = app.writeJSON(w, http.StatusOK, resp)
	if err != nil {
		return
	}
}

// GetSale returns one sale as json, by id
func (app *application) GetSale(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	orderID, _ := strconv.Atoi(id)

	order, err := app.DB.GetOrderByID(orderID)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	err = app.writeJSON(w, http.StatusOK, order)
	if err != nil {
		return
	}
}

// RefundCharge accepts a json payload and tries to refund a charge
func (app *application) RefundCharge(w http.ResponseWriter, r *http.Request) {
	var chargeToRefund struct {
		ID            int    `json:"id"`
		PaymentIntent string `json:"pi"`
		Amount        int    `json:"amount"`
		Currency      string `json:"currency"`
	}

	err := app.readJSON(w, r, &chargeToRefund)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	// validate
	card := cards.Card{
		Secret:   app.config.stripe.secret,
		Key:      app.config.stripe.key,
		Currency: chargeToRefund.Currency,
	}

	err = card.Refund(chargeToRefund.PaymentIntent, chargeToRefund.Amount)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	// update status in db
	err = app.DB.UpdateOrderStatus(chargeToRefund.ID, 2)
	if err != nil {
		err := app.badRequest(w, r, errors.New("the charge was refunded, but the database could not be updated"))
		if err != nil {
			return
		}
		return
	}

	var resp struct {
		Error   bool   `json:"error"`
		Message string `json:"message"`
	}
	resp.Error = false
	resp.Message = "Charge Refunded"

	err = app.writeJSON(w, http.StatusOK, resp)
	if err != nil {
		return
	}
}

// CancelSubscription is the handler to cancel a subscription
func (app *application) CancelSubscription(w http.ResponseWriter, r *http.Request) {
	var subToCancel struct {
		ID            int    `json:"id"`
		PaymentIntent string `json:"pi"`
		Currency      string `json:"currency"`
	}

	err := app.readJSON(w, r, &subToCancel)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	card := cards.Card{
		Secret:   app.config.stripe.secret,
		Key:      app.config.stripe.key,
		Currency: subToCancel.Currency,
	}

	err = card.CancelSubscriptions(subToCancel.PaymentIntent)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	// update status in db
	err = app.DB.UpdateOrderStatus(subToCancel.ID, 3)
	if err != nil {
		err := app.badRequest(w, r, errors.New("the subscription was cancelled, but the database could not be updated"))
		if err != nil {
			return
		}
		return
	}

	var resp struct {
		Error   bool   `json:"error"`
		Message string `json:"message"`
	}
	resp.Error = false
	resp.Message = "Subscription Cancelled"

	err = app.writeJSON(w, http.StatusOK, resp)
	if err != nil {
		return
	}
}

// AllUsers returns a JSON file listing all admin users
func (app *application) AllUsers(w http.ResponseWriter, r *http.Request) {
	allUsers, err := app.DB.GetAllUsers()
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	err = app.writeJSON(w, http.StatusOK, allUsers)
	if err != nil {
		return
	}
}

// OneUser gets one user by id (from the url) and returns it as JSON
func (app *application) OneUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID, _ := strconv.Atoi(id)

	user, err := app.DB.GetOneUser(userID)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	err = app.writeJSON(w, http.StatusOK, user)
	if err != nil {
		return
	}
}

// EditUser is the handler for adding or editing an existing user
func (app *application) EditUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID, _ := strconv.Atoi(id)

	var user models.User

	err := app.readJSON(w, r, &user)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	if userID > 0 {
		err = app.DB.EditUser(user)
		if err != nil {
			err := app.badRequest(w, r, err)
			if err != nil {
				return
			}
			return
		}
		if user.Password != "" {
			newHash, err := bcrypt.GenerateFromPassword([]byte(user.Password), 12)
			if err != nil {
				err := app.badRequest(w, r, err)
				if err != nil {
					return
				}
				return
			}
			err = app.DB.UpdatePasswordForUser(user, string(newHash))
			if err != nil {
				err := app.badRequest(w, r, err)
				if err != nil {
					return
				}
				return
			}
		}
	} else {
		newHash, err := bcrypt.GenerateFromPassword([]byte(user.Password), 12)
		if err != nil {
			err := app.badRequest(w, r, err)
			if err != nil {
				return
			}
			return
		}
		err = app.DB.AddUser(user, string(newHash))
		if err != nil {
			err := app.badRequest(w, r, err)
			if err != nil {
				return
			}
			return
		}
	}

	var resp struct {
		Error   bool   `json:"error"`
		Message string `json:"message"`
	}

	resp.Error = false
	err = app.writeJSON(w, http.StatusOK, resp)
	if err != nil {
		return
	}
}

// DeleteUser deletes a user, and all associated tokens, from the database
func (app *application) DeleteUser(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID, _ := strconv.Atoi(id)

	err := app.DB.DeleteUser(userID)
	if err != nil {
		err := app.badRequest(w, r, err)
		if err != nil {
			return
		}
		return
	}

	var resp struct {
		Error   bool   `json:"error"`
		Message string `json:"message"`
	}

	resp.Error = false
	err = app.writeJSON(w, http.StatusOK, resp)
	if err != nil {
		return
	}
}
