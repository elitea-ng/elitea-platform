-- Monthly report: an extension the SDK treats as code with no grammar.
SELECT p.name, count(*) AS tickets
FROM tickets t JOIN projects p ON p.id = t.project_id
WHERE t.created_at >= date_trunc('month', now())
GROUP BY p.name
ORDER BY tickets DESC;

SELECT u.email, sum(b.amount) AS spend
FROM billing b JOIN users u ON u.id = b.user_id
GROUP BY u.email
HAVING sum(b.amount) > 1000
ORDER BY spend DESC;
