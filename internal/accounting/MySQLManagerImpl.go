package accounting

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"github.com/IDN-Media/awards/internal/connector"
	"github.com/hyperjumptech/acccore"
	"github.com/olekukonko/tablewriter"
	"github.com/sirupsen/logrus"
	"math/big"
	"strings"
	"time"
)

// JOURNAL MANAGER ------------------------------------------------------------------
func NewMySQLJournalManager(repo connector.DBRepository) acccore.JournalManager {
	return &MySQLJournalManager{repo: repo}
}

// MySQLJournalManager implementation of JournalManager using Journal table in MySQL
type MySQLJournalManager struct {
	repo connector.DBRepository
}

// NewJournal will create new blank un-persisted journal
func (jm *MySQLJournalManager) NewJournal(ctx context.Context, ) acccore.Journal {
	return &acccore.BaseJournal{}
}

// PersistJournal will record a journal entry into database.
// It requires list of transactions for which each of the transaction MUST BE :
//    1.NOT BE PERSISTED. (the journal accountNumber is not exist in DB yet)
//    2.Pointing or owned by a PERSISTED Account
//    3.Each of this account must belong to the same Currency
//    4.Balanced. The total sum of DEBIT and total sum of CREDIT is equal.
//    5.No duplicate transaction that belongs to the same Account.
// If your database support 2 phased commit, you can make all balance changes in
// accounts and transactions. If your db do not support this, you can implement your own 2 phase commits mechanism
// on the CommitJournal and CancelJournal
func (jm *MySQLJournalManager) PersistJournal(ctx context.Context, journalToPersist acccore.Journal) error {
	// First we have to make sure that the journalToPersist is not yet in our database.
	// 1. Checking if the mandatories is not missing
	if journalToPersist == nil {
		return acccore.ErrJournalNil
	}
	if len(journalToPersist.GetJournalID()) == 0 {
		logrus.Errorf("error persisting journal. journal is missing the journalID")
		return acccore.ErrJournalMissingId
	}
	if len(journalToPersist.GetTransactions()) == 0 {
		logrus.Errorf("error persisting journal %s. journal contains no transactions.", journalToPersist.GetJournalID())
		return acccore.ErrJournalNoTransaction
	}
	if len(journalToPersist.GetCreateBy()) == 0 {
		logrus.Errorf("error persisting journal %s. journal author not known.", journalToPersist.GetJournalID())
		return acccore.ErrJournalMissingAuthor
	}

	// 2. Checking if the journal ID must not in the Database (already persisted)
	//    SQL HINT : SELECT COUNT(*) FROM JOURNAL WHERE JOURNAL.ID = {journalToPersist.GetJournalID()}
	//    If COUNT(*) is > 0 return error
	j, err := jm.repo.GetJournal(ctx,journalToPersist.GetJournalID())
	if err == nil || j != nil {
		logrus.Errorf("error persisting journal %s. journal already exist.", journalToPersist.GetJournalID())
		return acccore.ErrJournalAlreadyPersisted
	}

	// 3. Make sure all journal transactions are IDed.
	for idx, trx := range journalToPersist.GetTransactions() {
		if len(trx.GetTransactionID()) == 0 {
			logrus.Errorf("error persisting journal %s. transaction %d is missing transactionID.", journalToPersist.GetJournalID(), idx)
			return acccore.ErrJournalTransactionMissingID
		}
	}

	// 4. Make sure all journal transactions are not persisted.
	for idx, trx := range journalToPersist.GetTransactions() {
		t, err := jm.repo.GetTransaction(ctx, trx.GetTransactionID())
		if err == nil || t != nil {
			logrus.Errorf("error persisting journal %s. transaction %d is already exist.", journalToPersist.GetJournalID(), idx)
			return acccore.ErrJournalTransactionAlreadyPersisted
		}
	}

	// 5. Make sure transactions are balanced.
	var creditSum, debitSum int64
	for _, trx := range journalToPersist.GetTransactions() {
		if trx.GetTransactionType() == acccore.DEBIT {
			debitSum += trx.GetAmount()
		}
		if trx.GetTransactionType() == acccore.CREDIT {
			creditSum += trx.GetAmount()
		}
	}
	if creditSum != debitSum {
		logrus.Errorf("error persisting journal %s. debit (%d) != credit (%d). journal not balance", journalToPersist.GetJournalID(), debitSum, creditSum)
		return acccore.ErrJournalNotBalance
	}

	// 6. Make sure transactions account are not appear twice in the journal
	accountDupCheck := make(map[string]bool)
	for _, trx := range journalToPersist.GetTransactions() {
		if _, exist := accountDupCheck[trx.GetAccountNumber()]; exist {
			logrus.Errorf("error persisting journal %s. multiple transaction belong to the same account (%s)", journalToPersist.GetJournalID(), trx.GetAccountNumber())
			return acccore.ErrJournalTransactionAccountDuplicate
		}
		accountDupCheck[trx.GetAccountNumber()] = true
	}

	// 7. Make sure transactions are all belong to existing accounts
	for _, trx := range journalToPersist.GetTransactions() {
		account, err := jm.repo.GetAccount(ctx, trx.GetAccountNumber())
		if err!=nil || account == nil {
			logrus.Errorf("error persisting journal %s. theres a transaction belong to non existent account (%s)", journalToPersist.GetJournalID(), trx.GetAccountNumber())
			return acccore.ErrJournalTransactionAccountNotPersist
		}
	}

	// 8. Make sure transactions are all have the same currency
	var currency string
	for idx, trx := range journalToPersist.GetTransactions() {
		account, err := jm.repo.GetAccount(ctx, trx.GetAccountNumber())
		if err != nil || account == nil {
			return acccore.ErrAccountIdNotFound
		}
		cur := account.CurrencyCode
		if idx == 0 {
			currency = cur
		} else {
			if cur != currency {
				logrus.Errorf("error persisting journal %s. transactions here uses account with different currencies", journalToPersist.GetJournalID())
				return acccore.ErrJournalTransactionMixCurrency
			}
		}
	}

	// 9. If this is a reversal journal, make sure the journal being reversed have not been reversed before.
	if journalToPersist.GetReversedJournal() != nil {
		reversed, err := jm.IsJournalIdReversed(ctx, journalToPersist.GetJournalID())
		if err != nil {
			return err
		}
		if reversed {
			logrus.Errorf("error persisting journal %s. this journal try to make reverse transaction on journals thats already reversed %s", journalToPersist.GetJournalID(), journalToPersist.GetJournalID())
			return acccore.ErrJournalCanNotDoubleReverse
		}
	}

	// ALL is OK. So lets start persisting.

	// BEGIN transaction
	tx, err := jm.repo.DB().BeginTxx(ctx, &sql.TxOptions{
		// todo investigate the use of this.
		Isolation: 0,
		ReadOnly:  false,
	})
	if err != nil {
		logrus.Errorf("error creating transaction. got %s", err.Error())
		return err
	}

	// 1. Save the Journal
	journalToInsert := &connector.JournalRecord{
		JournalID:         journalToPersist.GetJournalID(),
		JournalingTime:    time.Now(),
		Description:       journalToPersist.GetDescription(),
		IsReversal:        false,
		ReversedJournalId: "",
		TotalAmount:       creditSum,
		CreatedAt:         time.Now(),
		CreatedBy:         journalToPersist.GetCreateBy(),
	}

	if journalToPersist.GetReversedJournal() != nil {
		journalToInsert.ReversedJournalId = journalToPersist.GetReversedJournal().GetJournalID()
		journalToInsert.IsReversal = true
	}

	journalId, err := jm.repo.InsertJournal(ctx, journalToInsert)
	if err != nil {
		logrus.Errorf("error inserting new journal %s . got %s. rolling back transaction.", journalToInsert.JournalID, err.Error())
		err=tx.Rollback()
		if err != nil {
			logrus.Errorf("error rolling back transaction. got %s", err.Error())
		}
		return err
	}

	// 2 Save the Transactions
	for _, trx := range journalToPersist.GetTransactions() {
		transactionToInsert := &connector.TransactionRecord{
			TransactionID: trx.GetTransactionID(),
			TransactionTime: trx.GetTransactionTime(),
			AccountNumber: trx.GetAccountNumber(),
			JournalID:     journalId,
			Description:   trx.GetDescription(),
			//Alignment:     string(trx.GetTransactionType()),
			Amount:        trx.GetAmount(),
			Balance:       trx.GetAccountBalance(),
			CreatedAt:     time.Now(),
			CreatedBy:     trx.GetCreateBy(),
		}

		if trx.GetTransactionType() == acccore.DEBIT {
			transactionToInsert.Alignment = "DEBIT"
		} else {
			transactionToInsert.Alignment = "CREDIT"
		}

		account, err := jm.repo.GetAccount(ctx, trx.GetAccountNumber())
		if err != nil {
			logrus.Errorf("error retrieving account %s in transaction. got %s. rolling back transaction.", trx.GetAccountNumber(), err.Error())
			err=tx.Rollback()
			if err != nil {
				logrus.Errorf("error rolling back transaction. got %s", err.Error())
			}
			return err
		}
		balance, accountTrxType := account.Balance, account.Alignment

		newBalance := int64(0)
		if transactionToInsert.Alignment == accountTrxType {
			newBalance = balance + transactionToInsert.Amount
		} else {
			newBalance = balance - transactionToInsert.Amount
		}
		transactionToInsert.Balance = newBalance

		_, err = jm.repo.InsertTransaction(ctx, transactionToInsert)
		if err != nil {
			logrus.Errorf("error inserting new transaction %s in transaction. got %s. rolling back transaction.", transactionToInsert.TransactionID, err.Error())
			err=tx.Rollback()
			if err != nil {
				logrus.Errorf("error rolling back transaction. got %s", err.Error())
			}
			return err
		}

		// Update Account Balance.
		// UPDATE ACCOUNT SET BALANCE = {newBalance},  UPDATEBY = {trx.GetCreateBy()}, UPDATE_TIME = {time.Now()} WHERE ACCOUNT_ID = {trx.GetAccountNumber()}
		account.Balance = newBalance
		account.UpdatedAt = time.Now()
		account.UpdatedBy = trx.GetCreateBy()
		err = jm.repo.UpdateAccount(ctx, account)
		if err != nil {
			logrus.Errorf("error updating account %s in transaction. got %s. rolling back transaction.", account.AccountNumber, err.Error())
			err=tx.Rollback()
			if err != nil {
				logrus.Errorf("error rolling back transaction. got %s", err.Error())
			}
			return err
		}
	}

	// COMMIT transaction
	err = tx.Commit()
	if err != nil {
		logrus.Errorf("error commiting transaction. got %s", err.Error())
		return err
	}

	return nil
}

// CommitJournal will commit the journal into the system
// Only non committed journal can be committed.
// use this if the implementation database do not support 2 phased commit.
// if your database support 2 phased commit, you should do all commit in the PersistJournal function
// and this function should simply return nil.
func (jm *MySQLJournalManager) CommitJournal(ctx context.Context, journalToCommit acccore.Journal) error {
	return nil
}

// CancelJournal Cancel a journal
// Only non committed journal can be committed.
// use this if the implementation database do not support 2 phased commit.
// if your database do not support 2 phased commit, you should do all roll back in the PersistJournal function
// and this function should simply return nil.
func (jm *MySQLJournalManager) CancelJournal(ctx context.Context, journalToCancel acccore.Journal) error {
	return nil
}

// IsJournalIdReversed check if the journal with specified ID has been reversed
func (jm *MySQLJournalManager) IsJournalIdReversed(ctx context.Context, journalId string) (bool, error) {
	// SELECT COUNT(*) FROM JOURNAL WHERE REVERSED_JOURNAL_ID = {journalID}
	// return false if COUNT = 0
	// return true if COUNT > 0
	journal, err := jm.repo.GetJournalByReversalID(ctx, journalId)
	if err != nil || journal == nil {
		return false, nil
	}
	return true, nil
}

// IsTransactionIdExist will check if an Transaction ID/number is exist in the database.
func (jm *MySQLJournalManager) IsJournalIdExist(ctx context.Context, journalId string) (bool, error) {
	journal, err := jm.repo.GetJournal(ctx, journalId)
	if err != nil || journal == nil {
		return false, nil
	}
	return true, nil
}

// GetJournalById retrieved a Journal information identified by its ID.
// the provided ID must be exactly the same, not uses the LIKE select expression.
func (jm *MySQLJournalManager) GetJournalById(ctx context.Context, journalId string) (acccore.Journal, error) {
	journal, err := jm.repo.GetJournal(ctx, journalId)
	if err != nil {
		return nil, err
	}
	ret := &acccore.BaseJournal{}
	ret.SetAmount(journal.TotalAmount).SetDescription(journal.Description).SetReversal(journal.IsReversal).
		SetJournalingTime(journal.JournalingTime).SetCreateBy(journal.CreatedBy).SetCreateTime(journal.CreatedAt).
		SetJournalID(journal.JournalID)

	if journal.IsReversal == true {
		reversed, err := jm.GetJournalById(ctx, journal.ReversedJournalId)
		if err != nil {
			return nil, acccore.ErrJournalLoadReversalInconsistent
		}
		ret.SetReversedJournal(reversed)
	}

	// Populate all transactions from DB.
	transactions := make([]acccore.Transaction, 0)
	trxs, err := jm.repo.ListTransactionByJournalID(ctx, journalId)
	if err != nil {
		return nil, err
	}
	for _, trx := range trxs {
		transaction := &acccore.BaseTransaction{}
		transaction.SetJournalID(trx.JournalID).SetTransactionTime(trx.TransactionTime).
			SetAccountNumber(trx.AccountNumber).SetTransactionID(trx.TransactionID).SetDescription(trx.Description).
			SetCreateTime(trx.CreatedAt).SetCreateBy(trx.CreatedBy).SetAccountBalance(trx.Balance).SetAmount(trx.Amount)
		if strings.ToUpper(trx.Alignment) == "DEBIT" {
			transaction.SetTransactionType(acccore.DEBIT)
		} else {
			transaction.SetTransactionType(acccore.CREDIT)
		}
		transactions = append(transactions, transaction)
	}
	ret.SetTransactions(transactions)

	return ret, nil
}

// ListJournals retrieve list of journals with transaction date between the `from` and `until` time range inclusive.
// This function uses pagination.
func (jm *MySQLJournalManager) ListJournals(ctx context.Context, from time.Time, until time.Time, request acccore.PageRequest) (acccore.PageResult, []acccore.Journal, error) {
	count, err := jm.repo.CountJournalByTimeRange(ctx, from, until)
	if err != nil {
		return acccore.PageResult{}, nil , err
	}
	pResult := acccore.PageResultFor(request, count)
	jRecords, err := jm.repo.ListJournal(ctx, "journaling_time", pResult.Offset, pResult.PageSize)
	if err != nil {
		return acccore.PageResult{}, nil , err
	}
	ret := make([]acccore.Journal, 0)
	for _, jrnl := range jRecords {
		journal, err := jm.GetJournalById(ctx, jrnl.JournalID)
		if err != nil {
			logrus.Errorf("Error while retrieving journal %s. got %s. skipping", jrnl.JournalID, err.Error());
		} else {
			ret = append(ret, journal)
		}
	}
	return pResult, ret, nil
}

// RenderJournal Render this journal into string for easy inspection
func (jm *MySQLJournalManager) RenderJournal(ctx context.Context, journal acccore.Journal) string {
	var buff bytes.Buffer
	table := tablewriter.NewWriter(&buff)
	table.SetHeader([]string{"TRX ID", "Account", "Description", "DEBIT", "CREDIT"})
	table.SetFooter([]string{"", "", "", fmt.Sprintf("%d", acccore.GetTotalDebit(journal)), fmt.Sprintf("%d", acccore.GetTotalCredit(journal))})

	for _, t := range journal.GetTransactions() {
		if t.GetTransactionType() == acccore.DEBIT {
			table.Append([]string{t.GetTransactionID(), t.GetAccountNumber(), t.GetDescription(), fmt.Sprintf("%d", t.GetAmount()), ""})
		}
	}
	for _, t := range journal.GetTransactions() {
		if t.GetTransactionType() == acccore.CREDIT {
			table.Append([]string{t.GetTransactionID(), t.GetAccountNumber(), t.GetDescription(), "", fmt.Sprintf("%d", t.GetAmount())})
		}
	}
	buff.WriteString(fmt.Sprintf("Journal Entry : %s\n", journal.GetJournalID()))
	buff.WriteString(fmt.Sprintf("Journal Date  : %s\n", journal.GetJournalingTime().String()))
	buff.WriteString(fmt.Sprintf("Description   : %s\n", journal.GetDescription()))
	table.Render()
	return buff.String()
}

// TRANSACTION MANAGER ------------------------------------------------------------------
func NewMySQLTransactionManager(repo connector.DBRepository) acccore.TransactionManager {
	return &MySQLTransactionManager{repo: repo}
}

// MySQLTransactionManager implementation of TransactionManager using Transaction table in MySQL
type MySQLTransactionManager struct {
	repo connector.DBRepository
}

// NewTransaction will create new blank un-persisted Transaction
func (am *MySQLTransactionManager) NewTransaction(ctx context.Context, ) acccore.Transaction {
	return &acccore.BaseTransaction{}
}

// IsTransactionIdExist will check if an Transaction ID/number is exist in the database.
func (am *MySQLTransactionManager) IsTransactionIdExist(ctx context.Context, id string) (bool, error) {
	tx, err := am.repo.GetTransaction(ctx,id)
	if err != nil {
		return false, err
	}
	if tx == nil {
		return false, nil
	}
	return true, nil
}

// GetTransactionById will retrieve one single transaction that identified by some ID
func (am *MySQLTransactionManager) GetTransactionById(ctx context.Context, id string) (acccore.Transaction, error) {
	tx, err := am.repo.GetTransaction(ctx,id)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, acccore.ErrTransactionNotFound
	}
	trx := &acccore.BaseTransaction{}
	trx.SetAmount(tx.Amount).SetAccountBalance(tx.Balance).SetCreateBy(tx.CreatedBy).SetCreateTime(tx.CreatedAt).
		SetDescription(tx.Description).SetTransactionID(tx.TransactionID).SetAccountNumber(tx.AccountNumber).
		SetTransactionTime(tx.TransactionTime).SetJournalID(tx.JournalID)

	if strings.ToUpper(tx.Alignment) == "DEBIT" {
		trx.SetTransactionType(acccore.DEBIT)
	} else {
		trx.SetTransactionType(acccore.CREDIT)
	}
	return trx, nil
}

// ListTransactionsWithAccount retrieves list of transactions that belongs to this account
// that transaction happens between the `from` and `until` time range.
// This function uses pagination
func (am *MySQLTransactionManager) ListTransactionsOnAccount(ctx context.Context, from time.Time, until time.Time, account acccore.Account, request acccore.PageRequest) (acccore.PageResult, []acccore.Transaction, error)  {
	count, err := am.repo.CountTransactionByAccountNumber(ctx, account.GetAccountNumber(), from, until)
	if err != nil {
		return acccore.PageResult{}, nil, err
	}
	pageResult := acccore.PageResultFor(request, count)
	records, err := am.repo.ListTransactionByAccountNumber(ctx, account.GetAccountNumber(), from, until, pageResult.Offset, pageResult.PageSize)
	if err != nil {
		return acccore.PageResult{}, nil, err
	}
	ret := make([]acccore.Transaction,0)
	for _, tx := range records {
		trx := &acccore.BaseTransaction{}
		trx.SetAmount(tx.Amount).SetAccountBalance(tx.Balance).SetCreateBy(tx.CreatedBy).SetCreateTime(tx.CreatedAt).
			SetDescription(tx.Description).SetTransactionID(tx.TransactionID).SetAccountNumber(tx.AccountNumber).
			SetTransactionTime(tx.TransactionTime).SetJournalID(tx.JournalID)

		if strings.ToUpper(tx.Alignment) == "DEBIT" {
			trx.SetTransactionType(acccore.DEBIT)
		} else {
			trx.SetTransactionType(acccore.CREDIT)
		}
		ret = append(ret, trx)
	}
	return pageResult, ret, nil
}

// RenderTransactionsOnAccount Render list of transaction been down on an account in a time span
func (am *MySQLTransactionManager) RenderTransactionsOnAccount(ctx context.Context, from time.Time, until time.Time, account acccore.Account, request acccore.PageRequest) (string, error) {
	result, transactions, err := am.ListTransactionsOnAccount(ctx, from, until, account, request)
	if err != nil {
		return "Error rendering", err
	}

	var buff bytes.Buffer
	table := tablewriter.NewWriter(&buff)
	table.SetHeader([]string{"TRX ID", "TIME", "JOURNAL ID", "Description", "DEBIT", "CREDIT", "BALANCE"})

	for _, t := range transactions {
		if t.GetTransactionType() == acccore.DEBIT {
			table.Append([]string{t.GetTransactionID(), t.GetTransactionTime().String(), t.GetJournalID(), t.GetDescription(), fmt.Sprintf("%d", t.GetAmount()), "", fmt.Sprintf("%d", t.GetAccountBalance())})
		}
		if t.GetTransactionType() == acccore.CREDIT {
			table.Append([]string{t.GetTransactionID(), t.GetTransactionTime().String(), t.GetJournalID(), t.GetDescription(), "", fmt.Sprintf("%d", t.GetAmount()), fmt.Sprintf("%d", t.GetAccountBalance())})
		}
	}

	buff.WriteString(fmt.Sprintf("Account Number    : %s\n", account.GetAccountNumber()))
	buff.WriteString(fmt.Sprintf("Account Name      : %s\n", account.GetName()))
	buff.WriteString(fmt.Sprintf("Description       : %s\n", account.GetDescription()))
	buff.WriteString(fmt.Sprintf("Currency          : %s\n", account.GetCurrency()))
	buff.WriteString(fmt.Sprintf("COA               : %s\n", account.GetCOA()))
	buff.WriteString(fmt.Sprintf("Transactions From : %s\n", from.String()))
	buff.WriteString(fmt.Sprintf("             To   : %s\n", until.String()))
	buff.WriteString(fmt.Sprintf("#Transactions     : %d\n", result.TotalEntries))
	buff.WriteString(fmt.Sprintf("Showing page      : %d/%d\n", result.Page, result.TotalPages))
	table.Render()
	return buff.String(), err
}

// ACCOUNT MANAGER ------------------------------------------------------------------
func NewMySQLAccountManager(repo connector.DBRepository) acccore.AccountManager {
	return &MySQLAccountManager{repo: repo}
}

// MySQLAccountManager implementation of AccountManager using Account table in MySQL
type MySQLAccountManager struct {
	repo connector.DBRepository
}

// NewAccount will create a new blank un-persisted account.
func (am *MySQLAccountManager) NewAccount(ctx context.Context ) acccore.Account {
	return &acccore.BaseAccount{}
}

// PersistAccount will save the account into database.
// will throw error if the account already persisted
func (am *MySQLAccountManager) PersistAccount(ctx context.Context, AccountToPersist acccore.Account) error {
	if len(AccountToPersist.GetAccountNumber()) == 0 {
		return acccore.ErrAccountMissingID
	}
	if len(AccountToPersist.GetName()) == 0 {
		return acccore.ErrAccountMissingName
	}
	if len(AccountToPersist.GetDescription()) == 0 {
		return acccore.ErrAccountMissingDescription
	}
	if len(AccountToPersist.GetCreateBy()) == 0 {
		return acccore.ErrAccountMissingCreator
	}

	curRec, err := am.repo.GetCurrency(ctx, AccountToPersist.GetCurrency())
	if err != nil {
		return err
	}
	if curRec == nil {
		logrus.Errorf("can not persist. currency do not exist %s", AccountToPersist.GetCurrency())
		return acccore.ErrCurrencyNotFound
	}

	ar := &connector.AccountRecord{
		AccountNumber: AccountToPersist.GetAccountNumber(),
		Name:          AccountToPersist.GetName(),
		CurrencyCode:  AccountToPersist.GetCurrency(),
		Description:   AccountToPersist.GetDescription(),
		// Alignment:     AccountToPersist.GetBaseTransactionType(),
		Balance:       AccountToPersist.GetBalance(),
		Coa:           AccountToPersist.GetCOA(),
		CreatedAt:     time.Now(),
		CreatedBy:     AccountToPersist.GetCreateBy(),
		UpdatedAt:     time.Now(),
		UpdatedBy:     AccountToPersist.GetUpdateBy(),
	}
	if AccountToPersist.GetBaseTransactionType() == acccore.DEBIT {
		ar.Alignment = "DEBIT"
	} else {
		ar.Alignment = "CREDIT"
	}

	_, err = am.repo.InsertAccount(ctx, ar)
	return err
}

// UpdateAccount will update the account database to reflect to the provided account information.
// This update account function will fail if the account ID/number is not existing in the database.
func (am *MySQLAccountManager) UpdateAccount(ctx context.Context, AccountToUpdate acccore.Account) error {

	if len(AccountToUpdate.GetAccountNumber()) == 0 {
		return acccore.ErrAccountMissingID
	}
	if len(AccountToUpdate.GetName()) == 0 {
		return acccore.ErrAccountMissingName
	}
	if len(AccountToUpdate.GetDescription()) == 0 {
		return acccore.ErrAccountMissingDescription
	}
	if len(AccountToUpdate.GetCreateBy()) == 0 {
		return acccore.ErrAccountMissingCreator
	}

	// First make sure that The account have never been created in DB.
	exist, err := am.IsAccountIdExist(ctx, AccountToUpdate.GetAccountNumber())
	if err != nil {
		return err
	}
	if !exist {
		return acccore.ErrAccountIsNotPersisted
	}

	ar := &connector.AccountRecord{
		AccountNumber: AccountToUpdate.GetAccountNumber(),
		Name:          AccountToUpdate.GetName(),
		CurrencyCode:  AccountToUpdate.GetCurrency(),
		Description:   AccountToUpdate.GetDescription(),
		// Alignment:     AccountToPersist.GetBaseTransactionType(),
		Balance:       AccountToUpdate.GetBalance(),
		Coa:           AccountToUpdate.GetCOA(),
		CreatedAt:     time.Now(),
		CreatedBy:     AccountToUpdate.GetCreateBy(),
		UpdatedAt:     time.Now(),
		UpdatedBy:     AccountToUpdate.GetUpdateBy(),
	}
	if AccountToUpdate.GetBaseTransactionType() == acccore.DEBIT {
		ar.Alignment = "DEBIT"
	} else {
		ar.Alignment = "CREDIT"
	}

	return am.repo.UpdateAccount(ctx, ar)

}

// IsAccountIdExist will check if an account ID/number is exist in the database.
func (am *MySQLAccountManager) IsAccountIdExist(ctx context.Context, id string) (bool, error) {
	ar, err := am.repo.GetAccount(ctx, id)
	if err != nil {
		return false, err
	}
	if ar == nil {
		return false, nil
	}
	return true, nil
}

// GetAccountById retrieve an account information by specifying the ID/number
func (am *MySQLAccountManager) GetAccountById(ctx context.Context, id string) (acccore.Account, error) {
	rec, err := am.repo.GetAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	ret := &acccore.BaseAccount{}
	ret.SetAccountNumber(rec.AccountNumber).SetDescription(rec.Description).SetCreateTime(rec.CreatedAt).
		SetCreateBy(rec.CreatedBy).SetCurrency(rec.CurrencyCode).SetCOA(rec.Coa).SetName(rec.Name).
		SetBalance(rec.Balance).SetUpdateBy(rec.UpdatedBy).SetUpdateTime(rec.UpdatedAt)

	if strings.ToUpper(rec.Alignment) == "DEBIT" {
		ret.SetBaseTransactionType(acccore.DEBIT)
	} else {
		ret.SetBaseTransactionType(acccore.CREDIT)
	}

	return ret, nil
}

// ListAccounts list all account in the database.
// This function uses pagination
func (am *MySQLAccountManager) ListAccounts(ctx context.Context, request acccore.PageRequest) (acccore.PageResult, []acccore.Account, error) {
	count, err := am.repo.CountAccounts(ctx)
	if err != nil {
		return acccore.PageResult{}, nil, err
	}
	pResult := acccore.PageResultFor(request, count)
	records, err := am.repo.ListAccount(ctx, "name", pResult.Offset, pResult.PageSize)
	if err != nil {
		return acccore.PageResult{}, nil, err
	}

	ret := make([]acccore.Account, 0)
	for _, rec := range records {
		bacc := &acccore.BaseAccount{}
		bacc.SetAccountNumber(rec.AccountNumber).SetDescription(rec.Description).SetCreateTime(rec.CreatedAt).
			SetCreateBy(rec.CreatedBy).SetCurrency(rec.CurrencyCode).SetCOA(rec.Coa).SetName(rec.Name).
			SetBalance(rec.Balance).SetUpdateBy(rec.UpdatedBy).SetUpdateTime(rec.UpdatedAt)

		if strings.ToUpper(rec.Alignment) == "DEBIT" {
			bacc.SetBaseTransactionType(acccore.DEBIT)
		} else {
			bacc.SetBaseTransactionType(acccore.CREDIT)
		}

		ret = append(ret, bacc)
	}

	return pResult, ret, nil
}

// ListAccountByCOA returns list of accounts that have the same COA number.
// This function uses pagination
func (am *MySQLAccountManager) ListAccountByCOA(ctx context.Context, coa string, request acccore.PageRequest) (acccore.PageResult, []acccore.Account, error) {
	count, err := am.repo.CountAccountByCoa(ctx, coa)
	if err != nil {
		return acccore.PageResult{}, nil, err
	}
	pResult := acccore.PageResultFor(request, count)
	records, err := am.repo.ListAccountByCoa(ctx, fmt.Sprintf("%s%%", coa), "name",  pResult.Offset, pResult.PageSize)
	if err != nil {
		return acccore.PageResult{}, nil, err
	}

	ret := make([]acccore.Account, 0)
	for _, rec := range records {
		bacc := &acccore.BaseAccount{}
		bacc.SetAccountNumber(rec.AccountNumber).SetDescription(rec.Description).SetCreateTime(rec.CreatedAt).
			SetCreateBy(rec.CreatedBy).SetCurrency(rec.CurrencyCode).SetCOA(rec.Coa).SetName(rec.Name).
			SetBalance(rec.Balance).SetUpdateBy(rec.UpdatedBy).SetUpdateTime(rec.UpdatedAt)

		if strings.ToUpper(rec.Alignment) == "DEBIT" {
			bacc.SetBaseTransactionType(acccore.DEBIT)
		} else {
			bacc.SetBaseTransactionType(acccore.CREDIT)
		}

		ret = append(ret, bacc)
	}
	return pResult, ret, nil
}

// FindAccounts returns list of accounts that have their name contains a substring of specified parameter.
// this search should  be case insensitive.
func (am *MySQLAccountManager) FindAccounts(ctx context.Context, nameLike string, request acccore.PageRequest) (acccore.PageResult, []acccore.Account, error) {
	count, err := am.repo.CountAccountByName(ctx, nameLike)
	if err != nil {
		return acccore.PageResult{}, nil, err
	}
	pResult := acccore.PageResultFor(request, count)
	records, err := am.repo.FindAccountByName(ctx, nameLike, "name",  pResult.Offset, pResult.PageSize)
	if err != nil {
		return acccore.PageResult{}, nil, err
	}

	ret := make([]acccore.Account, 0)
	for _, rec := range records {
		bacc := &acccore.BaseAccount{}
		bacc.SetAccountNumber(rec.AccountNumber).SetDescription(rec.Description).SetCreateTime(rec.CreatedAt).
			SetCreateBy(rec.CreatedBy).SetCurrency(rec.CurrencyCode).SetCOA(rec.Coa).SetName(rec.Name).
			SetBalance(rec.Balance).SetUpdateBy(rec.UpdatedBy).SetUpdateTime(rec.UpdatedAt)

		if strings.ToUpper(rec.Alignment) == "DEBIT" {
			bacc.SetBaseTransactionType(acccore.DEBIT)
		} else {
			bacc.SetBaseTransactionType(acccore.CREDIT)
		}

		ret = append(ret, bacc)
	}
	return pResult, ret, nil
}


func NewMySQLExchangeManager(repo connector.DBRepository) acccore.ExchangeManager {
	return &MySQLExchangeManager{repo: repo, commonDenominator: 1.0}
}

type MySQLExchangeManager struct {
	repo connector.DBRepository
	commonDenominator float64
}

// IsCurrencyExist will check in the exchange system for a currency existance
// non-existent currency means that the currency is not supported.
// error should be thrown if only there's an underlying error such as db error.
func (am *MySQLExchangeManager) IsCurrencyExist(context context.Context, currency string) (bool, error) {
	cr , err := am.repo.GetCurrency(context, currency)
	if err != nil {
		return false, err
	}
	if cr == nil {
		return false, nil
	}
	return true, nil
}
// GetDenom get the current common denominator used in the exchange
func (am *MySQLExchangeManager) GetDenom(context context.Context) *big.Float {
	return big.NewFloat(am.commonDenominator)
}
// SetDenom set the current common denominator value into the specified value
func (am *MySQLExchangeManager) SetDenom(context context.Context, denom *big.Float) {
	f, _ := denom.Float64()
	am.commonDenominator = f
}

// SetExchangeValueOf set the specified value as denominator value for that speciffic currency.
// This function should return error if the currency specified is not exist.
func (am *MySQLExchangeManager) SetExchangeValueOf(context context.Context, currency string, exchange *big.Float, author string) error {
	rec, err  := am.repo.GetCurrency(context, currency)
	if err != nil {
		return err
	}
	if rec == nil {
		rec := &connector.CurrenciesRecord{
			Code:      currency,
			Name:      currency,
			Exchange:  0,
			CreatedAt: time.Now(),
			CreatedBy: author,
			UpdatedAt: time.Now(),
			UpdatedBy: author,
		}
		_, err := am.repo.InsertCurrency(context, rec)
		return err
	}
	f,_ := exchange.Float64()
	rec.Exchange = f
	rec.UpdatedAt = time.Now()
	rec.UpdatedBy = author
	return am.repo.UpdateCurrency(context, rec)
}

// GetExchangeValueOf get the denominator value of the specified currency.
// Error should be returned if the specified currency is not exist.
func (am *MySQLExchangeManager) GetExchangeValueOf(context context.Context, currency string) (*big.Float, error) {
	if exist, err := am.IsCurrencyExist(context, currency); err == nil {
		if exist {
			rec, err := am.repo.GetCurrency(context, currency)
			if err != nil {
				return nil, err
			}
			return big.NewFloat(rec.Exchange), nil
		}
		return nil, acccore.ErrCurrencyNotFound
	} else {
		return nil, err
	}
}

// Get the currency exchange rate for exchanging between the two currency.
// if any of the currency is not exist, an error should be returned.
// if from and to currency is equal, this must return 1.0
func (am *MySQLExchangeManager) CalculateExchangeRate(context context.Context, fromCurrency, toCurrency string) (*big.Float, error) {
	from, err := am.GetExchangeValueOf(context, fromCurrency)
	if err != nil {
		return nil, err
	}
	to, err := am.GetExchangeValueOf(context, toCurrency)
	if err != nil {
		return nil, err
	}
	m1 := new(big.Float).Quo(am.GetDenom(context), from)
	m2 := new(big.Float).Mul(m1, to)
	m3 := new(big.Float).Quo(m2, am.GetDenom(context))
	return m3, nil
}
// Get the currency exchange value for the amount of fromCurrency into toCurrency.
// If any of the currency is not exist, an error should be returned.
// if from and to currency is equal, the returned amount must be equal to the amount in the argument.
func (am *MySQLExchangeManager) CalculateExchange(context context.Context, fromCurrency, toCurrency string, amount int64) (int64, error) {
	exchange, err := am.CalculateExchangeRate(context, fromCurrency, toCurrency)
	if err != nil {
		return 0, err
	}
	m1 := new(big.Float).Mul(exchange, big.NewFloat(float64(amount)))
	f, _ := m1.Float64()
	return int64(f), nil
}