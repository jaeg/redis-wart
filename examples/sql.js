db = sql.New("user:password@tcp(127.0.0.1:3306)/default", "mysql")
console.log(db.Ping())
results = db.Query("SELECT * FROM Test")
console.log("Result count", results.length)
console.log(results[0].Value1)