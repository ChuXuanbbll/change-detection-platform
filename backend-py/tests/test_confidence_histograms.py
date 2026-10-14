"""Confidence distribution contract for the comparison UI."""
import unittest
import numpy as np
from app.routers.detect import _compute_stats, _confidence_histograms


class ConfidenceHistogramTests(unittest.TestCase):
    def test_endpoints_including_zero_and_one(self):
        scores = np.array([0, 0.01, 0.5, 0.99, 1], dtype=np.float64)
        result = _confidence_histograms(np.array([0, 1, 0, 1, 1]), scores)
        self.assertEqual(len(result['bin_edges']), 21)
        self.assertEqual(result['all_valid_counts'][0], 2)
        self.assertEqual(result['all_valid_counts'][10], 1)
        self.assertEqual(result['all_valid_counts'][19], 2)
        self.assertEqual(result['changed_counts'][0], 1)
        self.assertEqual(result['changed_counts'][19], 2)
        self.assertEqual(result['changed_sample_count'], 3)
        self.assertEqual(result['all_valid_sample_count'], 5)

    def test_nonfinite_and_out_of_range_are_not_samples(self):
        scores = np.array([np.nan, np.inf, -np.inf, -0.1, 1.1, 0, 1])
        result = _confidence_histograms(np.ones(scores.shape), scores)
        self.assertEqual(sum(result['all_valid_counts']), 2)
        self.assertEqual(sum(result['changed_counts']), 2)

    def test_shared_validity_selection_and_existing_metrics(self):
        scores = np.array([0, 0.5, 1, 0.9])
        mask = np.array([0, 1, 1, 1])
        valid = np.array([True, True, True, False])
        result = _compute_stats(mask[valid], scores[valid])
        self.assertEqual(result['changed_pixels'], 2)
        self.assertEqual(result['total_pixels'], 3)
        self.assertAlmostEqual(result['change_ratio'], 2 / 3)
        self.assertEqual(result['mean_confidence'], 0.75)
        self.assertEqual(result['confidence_histograms']['all_valid_sample_count'], 3)
        self.assertEqual(result['confidence_histograms']['changed_sample_count'], 2)

    def test_empty_and_zero_change_populations(self):
        for scores in (np.array([]), np.array([0, 0.1, 0.2])):
            result = _confidence_histograms(np.zeros(scores.shape), scores)
            self.assertEqual(result['changed_counts'], [0] * 20)
            self.assertEqual(result['changed_sample_count'], 0)
            self.assertEqual(result['all_valid_sample_count'], scores.size)

    def test_nonfinite_scores_do_not_break_the_result_json(self):
        import json
        result = _compute_stats(np.array([1, 1, 0]), np.array([0.75, np.nan, np.inf]))
        self.assertEqual(result['mean_confidence_all'], 0.75)
        self.assertEqual(result['confidence_histograms']['all_valid_sample_count'], 1)
        json.dumps(result, allow_nan=False)


if __name__ == '__main__':
    unittest.main()
