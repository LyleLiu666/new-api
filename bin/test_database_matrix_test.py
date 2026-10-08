import unittest

from test_database_matrix import validate_environment, validate_results


class DatabaseMatrixGateTest(unittest.TestCase):
    def test_missing_database_configuration_fails_before_running_tests(self):
        with self.assertRaisesRegex(ValueError, "TEST_MYSQL_DSN"):
            validate_environment({})

    def test_complete_database_and_real_redis_configuration_is_accepted(self):
        validate_environment({
            "TEST_MYSQL_DSN": "mysql-test",
            "TEST_MYSQL_LOG_DSN": "mysql-log-test",
            "TEST_POSTGRES_DSN": "postgres-test",
            "TEST_POSTGRES_LOG_DSN": "postgres-log-test",
            "TEST_WS_MANAGER_REDIS_ADDR": "127.0.0.1:6379",
        })

    def test_skipped_database_is_not_reported_as_success(self):
        with self.assertRaisesRegex(ValueError, "skipped"):
            validate_results([{
                "Action": "skip", "Package": "service",
                "Test": "TestFixedPriceBillingDatabaseMatrix/mysql",
            }])

    def test_empty_or_partial_results_cannot_pass(self):
        for events in [[], [{"Action": "pass", "Package": "service"}]]:
            with self.subTest(events=events):
                with self.assertRaisesRegex(ValueError, "missing"):
                    validate_results(events)

    def test_failed_test_is_rejected_even_after_other_tests_pass(self):
        with self.assertRaisesRegex(ValueError, "failed"):
            validate_results([{"Action": "fail", "Package": "model", "Test": "TestQuota"}])

    def test_all_required_database_branches_and_redis_must_complete(self):
        events = []
        for package, test in [
            ("model", "TestMigrationSchemaStability"),
            ("model", "TestRequestPolicyDatabaseMatrix"),
            ("model", "TestCreditPackDatabaseMatrix"),
            ("model", "TestSubscriptionVersionDatabaseMatrix"),
            ("service", "TestFixedPriceBillingDatabaseMatrix"),
            ("controller", "TestPreConsumePolicyDatabaseMatrix"),
            ("controller", "TestRequestPolicyRoutingDatabaseMatrix"),
            ("controller", "TestDeleteRedemptionBatch"),
            ("controller", "TestCreditBillingDatabaseMatrix"),
        ]:
            for dialect in ["sqlite", "mysql", "postgres"]:
                events.append({"Action": "pass", "Package": f"github.com/QuantumNous/new-api/{package}", "Test": f"{test}/{dialect}"})
        events.append({"Action": "pass", "Package": "github.com/QuantumNous/new-api/pkg/wsmanager", "Test": "TestRedisChannelCloseEventsStayWithinDatabase"})
        validate_results(events)
        with self.assertRaisesRegex(ValueError, "missing"):
            validate_results(events[:-1])


if __name__ == "__main__":
    unittest.main()
